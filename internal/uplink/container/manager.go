package container

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const (
	defaultDockerBin   = "docker"
	defaultDockerImage = "jrottenberg/ffmpeg:6.0-alpine"
	maxFFmpegLogLength = 2000
)

var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)
var ffmpegOptionNotFound = regexp.MustCompile(`Option ([^\s.]+) not found`)
var ffmpegUnrecognizedOption = regexp.MustCompile(`Unrecognized option ['"]?([^'"\s]+)['"]?`)

var (
	defaultFFmpegGlobalArgs = []string{
		"-hide_banner",
	}
	defaultFFmpegInputArgs = []string{
		"-fflags", "+nobuffer",
		"-rtsp_transport", "tcp",
		"-rw_timeout", "15000000",
		"-stimeout", "15000000",
	}
	defaultFFmpegOutputArgs = []string{
		"-c", "copy",
		"-f", "mpegts",
		"-mpegts_flags", "+resend_headers",
		"-muxdelay", "0",
		"-muxpreload", "0",
	}
)

type Manager struct {
	dockerBin        string
	image            string
	configDir        string
	buildContext     string
	dockerfile       string
	ffmpegGlobalArgs []string
	ffmpegInputArgs  []string
	ffmpegOutputArgs []string
}

type Status struct {
	State    string
	ExitCode int
	Error    string
}

type StartErrorKind string

const (
	StartErrorKindUnsupportedOption StartErrorKind = "unsupported_option"
	StartErrorKindNetworkFailure    StartErrorKind = "network_failure"
	StartErrorKindUnknown           StartErrorKind = "unknown"
	StartErrorKindDockerFailure     StartErrorKind = "docker_failure"
)

type StartError struct {
	Kind       StartErrorKind
	Err        error
	FFmpegArgs []string
	Logs       string
	Summary    string
}

func (e *StartError) Error() string {
	if e == nil {
		return ""
	}
	var parts []string
	switch e.Kind {
	case StartErrorKindUnsupportedOption:
		if e.Summary != "" {
			parts = append(parts, e.Summary)
		} else {
			parts = append(parts, "ffmpeg unsupported option")
		}
	case StartErrorKindNetworkFailure:
		parts = append(parts, "ffmpeg network failure")
	case StartErrorKindDockerFailure:
		parts = append(parts, "docker run failure")
	default:
		parts = append(parts, "ffmpeg start failure")
	}
	if len(e.FFmpegArgs) > 0 {
		parts = append(parts, fmt.Sprintf("ffmpeg_args=%q", strings.Join(e.FFmpegArgs, " ")))
	}
	if e.Logs != "" {
		parts = append(parts, fmt.Sprintf("ffmpeg_logs=%q", e.Logs))
	}
	if e.Err != nil {
		parts = append(parts, fmt.Sprintf("err=%v", e.Err))
	}
	return strings.Join(parts, " ")
}

func (e *StartError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type Request struct {
	Name     string
	ProxyURL string
	SRTURL   string
}

func NewManagerFromEnv() *Manager {
	return &Manager{
		dockerBin:        getenv("UPLINK_DOCKER_BIN", defaultDockerBin),
		image:            getenv("UPLINK_DOCKER_IMAGE", defaultDockerImage),
		configDir:        getenv("UPLINK_DOCKER_CONFIG", ""),
		buildContext:     os.Getenv("UPLINK_DOCKER_BUILD_CONTEXT"),
		dockerfile:       os.Getenv("UPLINK_DOCKERFILE"),
		ffmpegGlobalArgs: parseArgsEnv("UPLINK_FFMPEG_GLOBAL_ARGS", defaultFFmpegGlobalArgs),
		ffmpegInputArgs:  parseArgsEnv("UPLINK_FFMPEG_INPUT_ARGS", defaultFFmpegInputArgs),
		ffmpegOutputArgs: parseArgsEnv("UPLINK_FFMPEG_OUTPUT_ARGS", defaultFFmpegOutputArgs),
	}
}

func (m *Manager) Start(ctx context.Context, req Request) (string, error) {
	if req.Name == "" {
		return "", fmt.Errorf("container name required")
	}
	if req.ProxyURL == "" {
		return "", fmt.Errorf("proxy url required")
	}
	if req.SRTURL == "" {
		return "", fmt.Errorf("srt url required")
	}
	if err := validateRequest(req); err != nil {
		return "", err
	}
	if err := m.ensureImage(ctx); err != nil {
		return "", fmt.Errorf("ensure docker image: %w", err)
	}
	_, _ = m.run(ctx, "rm", "-f", req.Name)

	containerID, logsOut, err := m.startContainer(ctx, req, m.ffmpegInputArgs)
	if err == nil {
		return containerID, nil
	}
	if logsOut != "" {
		option := unsupportedFFmpegOption(logsOut)
		if option != "" {
			optionFlag := option
			if !strings.HasPrefix(optionFlag, "-") {
				optionFlag = "-" + optionFlag
			}
			fallbackInputArgs := removeOptionWithValue(m.ffmpegInputArgs, optionFlag)
			if len(fallbackInputArgs) != len(m.ffmpegInputArgs) {
				log.Printf("ffmpeg in %q does not support %s; retrying without it", m.image, optionFlag)
				_, _ = m.run(ctx, "rm", "-f", req.Name)
				containerID, _, retryErr := m.startContainer(ctx, req, fallbackInputArgs)
				if retryErr == nil {
					return containerID, nil
				}
				return "", retryErr
			}
		}
	}
	return "", err
}

func (m *Manager) startContainer(ctx context.Context, req Request, inputArgs []string) (string, string, error) {
	ffmpegArgs := m.buildFFmpegArgs(req, inputArgs)
	runArgs := append([]string{"run", "-d", "--name", req.Name, "--network", "host", m.image}, ffmpegArgs...)
	runOut, err := m.run(ctx, runArgs...)
	if err != nil {
		return "", runOut, &StartError{
			Kind:       StartErrorKindDockerFailure,
			Err:        fmt.Errorf("start docker container: %w", err),
			FFmpegArgs: ffmpegArgs,
			Logs:       truncateString(strings.TrimSpace(runOut), maxFFmpegLogLength),
		}
	}
	containerID := strings.TrimSpace(runOut)
	if containerID == "" {
		return "", "", fmt.Errorf("start docker container: empty container id")
	}
	status, exitCode, stateErr, err := m.inspectState(ctx, containerID)
	if err != nil {
		return "", "", err
	}
	if status != "running" {
		logsOut, _ := m.run(ctx, "logs", "--tail", "200", containerID)
		logsSnippet := strings.TrimSpace(logsOut)
		kind, summary := classifyFFmpegLogs(logsSnippet)
		return "", logsOut, &StartError{
			Kind:       kind,
			Err:        fmt.Errorf("container %s not running (status=%s exitCode=%s stateError=%s)", containerID, status, exitCode, strings.TrimSpace(stateErr)),
			FFmpegArgs: ffmpegArgs,
			Logs:       truncateString(logsSnippet, maxFFmpegLogLength),
			Summary:    summary,
		}
	}
	return containerID, "", nil
}

func (m *Manager) inspectState(ctx context.Context, containerID string) (string, string, string, error) {
	inspectOut, err := m.run(ctx, "inspect", "--format", "{{.State.Status}}|{{.State.ExitCode}}|{{.State.Error}}", containerID)
	if err != nil {
		return "", "", "", fmt.Errorf("inspect docker container %s: %w", containerID, err)
	}
	inspectParts := strings.SplitN(strings.TrimSpace(inspectOut), "|", 3)
	if len(inspectParts) != 3 {
		return "", "", "", fmt.Errorf("inspect docker container %s: unexpected output %q", containerID, strings.TrimSpace(inspectOut))
	}
	return inspectParts[0], inspectParts[1], inspectParts[2], nil
}

func (m *Manager) buildFFmpegArgs(req Request, inputArgs []string) []string {
	normalizedInputArgs := normalizeInputArgs(req.ProxyURL, inputArgs)
	args := make([]string, 0, len(m.ffmpegGlobalArgs)+len(normalizedInputArgs)+len(m.ffmpegOutputArgs)+4)
	args = append(args, m.ffmpegGlobalArgs...)
	args = append(args, normalizedInputArgs...)
	args = append(args, "-i", req.ProxyURL)
	args = append(args, m.ffmpegOutputArgs...)
	args = append(args, req.SRTURL)
	return args
}

func normalizeInputArgs(proxyURL string, inputArgs []string) []string {
	args := append([]string(nil), inputArgs...)
	switch urlScheme(proxyURL) {
	case "file":
		args = prependIfMissing(args, "-re")
		args = removeOptionWithValue(args, "-rtsp_transport")
		args = removeOptionWithValue(args, "-stimeout")
	case "rtsp":
		// keep args
	default:
		args = removeOptionWithValue(args, "-rtsp_transport")
		args = removeOptionWithValue(args, "-stimeout")
	}
	return args
}

func prependIfMissing(args []string, value string) []string {
	for _, arg := range args {
		if arg == value {
			return args
		}
	}
	return append([]string{value}, args...)
}

func removeOptionWithValue(args []string, option string) []string {
	if len(args) == 0 {
		return args
	}
	filtered := make([]string, 0, len(args))
	skipNext := false
	for i, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == option {
			if i+1 < len(args) {
				skipNext = true
			}
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered
}

func truncateString(value string, maxLen int) string {
	if maxLen <= 0 || value == "" {
		return value
	}
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen] + "...(truncated)"
}

func unsupportedFFmpegOption(logs string) string {
	if matches := ffmpegOptionNotFound.FindStringSubmatch(logs); len(matches) > 1 {
		return matches[1]
	}
	if matches := ffmpegUnrecognizedOption.FindStringSubmatch(logs); len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func classifyFFmpegLogs(logs string) (StartErrorKind, string) {
	if logs == "" {
		return StartErrorKindUnknown, ""
	}
	if option := unsupportedFFmpegOption(logs); option != "" {
		return StartErrorKindUnsupportedOption, fmt.Sprintf("ffmpeg unsupported option: %s", option)
	}
	networkIndicators := []string{
		"Connection refused",
		"Connection timed out",
		"Network is unreachable",
		"No route to host",
		"Connection reset by peer",
		"Could not resolve host",
		"Server returned 404",
		"HTTP error",
	}
	for _, indicator := range networkIndicators {
		if strings.Contains(logs, indicator) {
			return StartErrorKindNetworkFailure, ""
		}
	}
	return StartErrorKindUnknown, ""
}

func validateRequest(req Request) error {
	if err := validateURLScheme(req.ProxyURL, "rtsp", "file"); err != nil {
		return fmt.Errorf("proxy url invalid: %w", err)
	}
	if err := validateURLScheme(req.SRTURL, "srt"); err != nil {
		return fmt.Errorf("srt url invalid: %w", err)
	}
	return nil
}

func validateURLScheme(rawURL string, schemes ...string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	scheme := strings.ToLower(parsed.Scheme)
	for _, expected := range schemes {
		if scheme == expected {
			if parsed.Host == "" && expected != "file" {
				return fmt.Errorf("missing host")
			}
			return nil
		}
	}
	return fmt.Errorf("expected scheme %q, got %q", strings.Join(schemes, ","), parsed.Scheme)
}

func urlScheme(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Scheme)
}

func (m *Manager) InspectStatus(ctx context.Context, name string) (Status, error) {
	if name == "" {
		return Status{}, fmt.Errorf("container name required")
	}
	inspectOut, err := m.run(ctx, "inspect", "--format", "{{.State.Status}}|{{.State.ExitCode}}|{{.State.Error}}", name)
	if err != nil {
		return Status{}, fmt.Errorf("inspect docker container %s: %w", name, err)
	}
	inspectParts := strings.SplitN(strings.TrimSpace(inspectOut), "|", 3)
	if len(inspectParts) != 3 {
		return Status{}, fmt.Errorf("inspect docker container %s: unexpected output %q", name, strings.TrimSpace(inspectOut))
	}
	exitCode := 0
	if _, parseErr := fmt.Sscanf(inspectParts[1], "%d", &exitCode); parseErr != nil {
		return Status{}, fmt.Errorf("inspect docker container %s: invalid exit code %q", name, inspectParts[1])
	}
	return Status{
		State:    inspectParts[0],
		ExitCode: exitCode,
		Error:    inspectParts[2],
	}, nil
}

func (m *Manager) ensureImage(ctx context.Context) error {
	_, err := m.runWithEnv(ctx, []string{"image", "inspect", m.image}, nil)
	if err == nil {
		return nil
	}
	if m.buildContext == "" && m.dockerfile == "" {
		log.Printf("docker image not found; pulling %q", m.image)
		if _, pullErr := m.run(ctx, "pull", m.image); pullErr != nil {
			return fmt.Errorf("pull docker image %q: %w", m.image, pullErr)
		}
		log.Printf("docker image ready via pull: %q", m.image)
		return nil
	}
	buildContext := m.buildContext
	if buildContext == "" {
		buildContext = "."
	}
	args := []string{"build", "-t", m.image}
	if m.dockerfile != "" {
		args = append(args, "-f", m.dockerfile)
	}
	args = append(args, buildContext)
	log.Printf("docker image not found; building %q with context %q", m.image, buildContext)
	if _, buildErr := m.run(ctx, args...); buildErr != nil {
		return fmt.Errorf("build docker image %q: %w", m.image, buildErr)
	}
	log.Printf("docker image ready via build: %q", m.image)
	return nil
}

func (m *Manager) Stop(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("container name required")
	}
	_, err := m.run(ctx, "rm", "-f", name)
	if err != nil {
		return fmt.Errorf("remove docker container: %w", err)
	}
	return nil
}

func NameForCentralPath(path string) string {
	sanitized := strings.Trim(strings.TrimSpace(path), "/")
	if sanitized == "" {
		sanitized = "default"
	}
	sanitized = strings.ReplaceAll(sanitized, "/", "-")
	sanitized = invalidNameChars.ReplaceAllString(sanitized, "-")
	return fmt.Sprintf("cam-bus-uplink-%s", sanitized)
}

func (m *Manager) run(ctx context.Context, args ...string) (string, error) {
	out, err := m.runWithEnv(ctx, args, nil)
	if err != nil && m.configDir == "" && strings.Contains(out, "error getting credentials") {
		fallbackDir := "/tmp/cam-bus-docker-config"
		if mkErr := os.MkdirAll(fallbackDir, 0o700); mkErr == nil {
			fallbackEnv := []string{"DOCKER_CONFIG=" + fallbackDir}
			fallbackOut, fallbackErr := m.runWithEnv(ctx, args, fallbackEnv)
			if fallbackErr == nil {
				return fallbackOut, nil
			}
		}
	}
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return out, nil
}

func (m *Manager) runWithEnv(ctx context.Context, args []string, extraEnv []string) (string, error) {
	cmd := exec.CommandContext(ctx, m.dockerBin, args...)
	if m.configDir != "" {
		extraEnv = append(extraEnv, "DOCKER_CONFIG="+m.configDir)
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		return output, err
	}
	return output, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseArgsEnv(key string, def []string) []string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return strings.Fields(v)
	}
	return def
}
