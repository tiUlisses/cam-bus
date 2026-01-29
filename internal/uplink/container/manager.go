package container

import (
	"context"
	"fmt"
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
	dockerBin string
	image     string
}

type Request struct {
	Name     string
	ProxyURL string
	SRTURL   string
}

func NewManagerFromEnv() *Manager {
	return &Manager{
		dockerBin: getenv("UPLINK_DOCKER_BIN", defaultDockerBin),
		image:     getenv("UPLINK_DOCKER_IMAGE", defaultDockerImage),
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
	_ = m.run(ctx, "rm", "-f", req.Name)
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
	cmd := exec.CommandContext(ctx, m.dockerBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
