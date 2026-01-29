package container

import (
	"context"
	"fmt"
	"log"
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

func (m *Manager) Start(ctx context.Context, req Request) error {
	if req.Name == "" {
		return fmt.Errorf("container name required")
	}
	if req.ProxyURL == "" {
		return fmt.Errorf("proxy url required")
	}
	if req.SRTURL == "" {
		return fmt.Errorf("srt url required")
	}
	if err := m.ensureImage(ctx); err != nil {
		return fmt.Errorf("ensure docker image: %w", err)
	}
	_, _ = m.run(ctx, "rm", "-f", req.Name)
	_, err := m.run(ctx, "run", "-d", "--name", req.Name, "--network", "host",
		m.image, "ffmpeg",
		"-rtsp_transport", "tcp",
		"-i", req.ProxyURL,
		"-c", "copy",
		"-f", "mpegts",
		req.SRTURL,
	)
	if err != nil {
		return fmt.Errorf("start docker container: %w", err)
	}
	return nil
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
