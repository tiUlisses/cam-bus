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
)

var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

type Manager struct {
	dockerBin    string
	image        string
	configDir    string
	buildContext string
	dockerfile   string
}

type Status struct {
	State    string
	ExitCode int
	Error    string
}

type Request struct {
	Name     string
	ProxyURL string
	SRTURL   string
}

func NewManagerFromEnv() *Manager {
	return &Manager{
		dockerBin:    getenv("UPLINK_DOCKER_BIN", defaultDockerBin),
		image:        getenv("UPLINK_DOCKER_IMAGE", defaultDockerImage),
		configDir:    getenv("UPLINK_DOCKER_CONFIG", ""),
		buildContext: os.Getenv("UPLINK_DOCKER_BUILD_CONTEXT"),
		dockerfile:   os.Getenv("UPLINK_DOCKERFILE"),
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
	ffmpegArgs := buildFFmpegArgs(req)
	runArgs := append([]string{"run", "-d", "--name", req.Name, "--network", "host", m.image}, ffmpegArgs...)
	runOut, err := m.run(ctx, runArgs...)
	if err != nil {
		return "", fmt.Errorf("start docker container: %w", err)
	}
	containerID := strings.TrimSpace(runOut)
	if containerID == "" {
		return "", fmt.Errorf("start docker container: empty container id")
	}
	inspectOut, err := m.run(ctx, "inspect", "--format", "{{.State.Status}}|{{.State.ExitCode}}|{{.State.Error}}", containerID)
	if err != nil {
		return "", fmt.Errorf("inspect docker container %s: %w", containerID, err)
	}
	inspectParts := strings.SplitN(strings.TrimSpace(inspectOut), "|", 3)
	if len(inspectParts) != 3 {
		return "", fmt.Errorf("inspect docker container %s: unexpected output %q", containerID, strings.TrimSpace(inspectOut))
	}
	status := inspectParts[0]
	exitCode := inspectParts[1]
	stateErr := inspectParts[2]
	if status != "running" {
		logsOut, _ := m.run(ctx, "logs", "--tail", "50", containerID)
		logsSnippet := strings.TrimSpace(logsOut)
		return "", fmt.Errorf("container %s not running (status=%s exitCode=%s stateError=%s logs=%s)", containerID, status, exitCode, strings.TrimSpace(stateErr), logsSnippet)
	}
	return containerID, nil
}

func buildFFmpegArgs(req Request) []string {
	return []string{
		"-rtsp_transport", "tcp",
		"-i", req.ProxyURL,
		"-c", "copy",
		"-f", "mpegts",
		req.SRTURL,
	}
}

func validateRequest(req Request) error {
	if err := validateURLScheme(req.ProxyURL, "rtsp"); err != nil {
		return fmt.Errorf("proxy url invalid: %w", err)
	}
	if err := validateURLScheme(req.SRTURL, "srt"); err != nil {
		return fmt.Errorf("srt url invalid: %w", err)
	}
	return nil
}

func validateURLScheme(rawURL, scheme string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if strings.ToLower(parsed.Scheme) != scheme {
		return fmt.Errorf("expected scheme %q, got %q", scheme, parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
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
