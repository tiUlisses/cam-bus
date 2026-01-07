package uplink

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sua-org/cam-bus/internal/core"
)

const (
	defaultProxyRTSPBase = "rtsp://localhost:8554"
	defaultFFmpegBin     = "ffmpeg"
	defaultSRTPacketSize = 1316
	defaultSRTPort       = 8890
)

type Manager struct {
	ffmpegBin          string
	proxyRTSPBase      string
	defaultCentralHost string
	defaultSRTPort     int
	mu                 sync.Mutex
	uplinks            map[string]*uplinkProcess
}

type uplinkProcess struct {
	cameraKey string
	payload   Request
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	ttlTimer  *time.Timer
}

type Request struct {
	CameraID       string `json:"cameraId"`
	ProxyPath      string `json:"proxyPath"`
	CentralHost    string `json:"centralHost"`
	CentralSRTPort int    `json:"centralSrtPort"`
	CentralPath    string `json:"centralPath"`
	TTLSeconds     int    `json:"ttlSeconds"`
}

func NewManagerFromEnv() *Manager {
	return &Manager{
		ffmpegBin:          getenv("UPLINK_FFMPEG_BIN", defaultFFmpegBin),
		proxyRTSPBase:      strings.TrimSuffix(getenv("UPLINK_PROXY_RTSP_BASE", defaultProxyRTSPBase), "/"),
		defaultCentralHost: strings.TrimSpace(os.Getenv("UPLINK_CENTRAL_HOST")),
		defaultSRTPort:     getenvInt("UPLINK_CENTRAL_SRT_PORT", defaultSRTPort),
		uplinks:            make(map[string]*uplinkProcess),
	}
}

func (r *Request) Normalize() {
	r.CameraID = strings.TrimSpace(r.CameraID)
	r.ProxyPath = strings.Trim(strings.TrimSpace(r.ProxyPath), "/")
	r.CentralHost = strings.TrimSpace(r.CentralHost)
	r.CentralPath = strings.Trim(strings.TrimSpace(r.CentralPath), "/")
}

func (r Request) Validate() error {
	if r.CameraID == "" {
		return errors.New("cameraId required")
	}
	return nil
}

func (m *Manager) Start(req Request) error {
	req = m.applyDefaults(req)
	if err := validateStart(req); err != nil {
		return err
	}
	cameraKey := keyFor(req)
	return m.startUplink(cameraKey, req)
}

func (m *Manager) Stop(req Request) error {
	req = m.applyDefaults(req)
	cameraKey := keyFor(req)
	return m.stopUplink(cameraKey, "stop command")
}

func (m *Manager) StopByCamera(info core.CameraInfo) {
	candidates := make(map[string]struct{})
	if centralPath := strings.Trim(strings.TrimSpace(info.CentralPath), "/"); centralPath != "" {
		candidates[centralPath] = struct{}{}
	}
	if proxyPath := strings.Trim(strings.TrimSpace(info.ProxyPath), "/"); proxyPath != "" {
		candidates[proxyPath] = struct{}{}
	}
	if cameraID := strings.TrimSpace(info.DeviceID); cameraID != "" {
		candidates[cameraID] = struct{}{}
	}
	if len(candidates) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for key := range candidates {
		if proc, ok := m.uplinks[key]; ok {
			m.stopProcess(proc, "camera cleanup")
			delete(m.uplinks, key)
		}
	}
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, proc := range m.uplinks {
		m.stopProcess(proc, "shutdown")
		delete(m.uplinks, key)
	}
}

func (m *Manager) applyDefaults(req Request) Request {
	if req.ProxyPath == "" {
		req.ProxyPath = req.CameraID
	}
	if req.CentralPath == "" {
		req.CentralPath = req.ProxyPath
	}
	if req.CentralHost == "" {
		req.CentralHost = m.defaultCentralHost
	}
	if req.CentralSRTPort <= 0 {
		req.CentralSRTPort = m.defaultSRTPort
	}
	return req
}

func validateStart(req Request) error {
	if req.CameraID == "" {
		return errors.New("cameraId required")
	}
	if req.ProxyPath == "" {
		return errors.New("proxyPath required")
	}
	if req.CentralHost == "" {
		return errors.New("centralHost required")
	}
	if req.CentralPath == "" {
		return errors.New("centralPath required")
	}
	return nil
}

func keyFor(req Request) string {
	if req.CentralPath != "" {
		return req.CentralPath
	}
	return req.CameraID
}

func (m *Manager) startUplink(cameraKey string, req Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.uplinks[cameraKey]; ok {
		if sameRequest(existing.payload, req) {
			log.Printf("[uplink] already running for %s, refreshing TTL", cameraKey)
			m.refreshTTL(existing, req.TTLSeconds)
			return nil
		}
		m.stopProcess(existing, "restarting with new payload")
	}

	proxyURL := fmt.Sprintf("%s/%s", m.proxyRTSPBase, strings.TrimPrefix(req.ProxyPath, "/"))
	srtURL := buildSRTURL(req.CentralHost, req.CentralSRTPort, req.CentralPath)

	cmdCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(cmdCtx, m.ffmpegBin,
		"-rtsp_transport", "tcp",
		"-i", proxyURL,
		"-c", "copy",
		"-f", "mpegts",
		srtURL,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	proc := &uplinkProcess{
		cameraKey: cameraKey,
		payload:   req,
		cancel:    cancel,
		cmd:       cmd,
	}
	m.uplinks[cameraKey] = proc
	m.refreshTTL(proc, req.TTLSeconds)

	go func() {
		err := cmd.Wait()
		if err != nil {
			log.Printf("[uplink] ffmpeg exited for %s: %v", cameraKey, err)
		} else {
			log.Printf("[uplink] ffmpeg exited for %s", cameraKey)
		}
		m.mu.Lock()
		if current, ok := m.uplinks[cameraKey]; ok && current == proc {
			delete(m.uplinks, cameraKey)
		}
		m.mu.Unlock()
	}()

	log.Printf("[uplink] started for %s -> %s", cameraKey, srtURL)
	return nil
}

func (m *Manager) stopUplink(cameraKey, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	proc, ok := m.uplinks[cameraKey]
	if !ok {
		return fmt.Errorf("uplink not running")
	}
	m.stopProcess(proc, reason)
	delete(m.uplinks, cameraKey)
	return nil
}

func (m *Manager) stopProcess(proc *uplinkProcess, reason string) {
	if proc.ttlTimer != nil {
		proc.ttlTimer.Stop()
	}
	log.Printf("[uplink] stopping %s: %s", proc.cameraKey, reason)
	proc.cancel()
}

func (m *Manager) refreshTTL(proc *uplinkProcess, ttlSeconds int) {
	if proc.ttlTimer != nil {
		proc.ttlTimer.Stop()
		proc.ttlTimer = nil
	}
	if ttlSeconds <= 0 {
		return
	}
	proc.ttlTimer = time.AfterFunc(time.Duration(ttlSeconds)*time.Second, func() {
		if err := m.stopUplink(proc.cameraKey, "ttl expired"); err != nil {
			log.Printf("[uplink] ttl stop failed for %s: %v", proc.cameraKey, err)
		}
	})
}

func sameRequest(a, b Request) bool {
	return a.CameraID == b.CameraID &&
		a.ProxyPath == b.ProxyPath &&
		a.CentralHost == b.CentralHost &&
		normalizePort(a.CentralSRTPort) == normalizePort(b.CentralSRTPort) &&
		a.CentralPath == b.CentralPath
}

func normalizePort(port int) int {
	if port <= 0 {
		return defaultSRTPort
	}
	return port
}

func buildSRTURL(host string, port int, path string) string {
	if port <= 0 {
		port = defaultSRTPort
	}
	streamID := fmt.Sprintf("publish:%s", path)
	query := fmt.Sprintf("streamid=%s&pkt_size=%d", streamID, defaultSRTPacketSize)

	u := url.URL{
		Scheme:   "srt",
		Host:     fmt.Sprintf("%s:%d", host, port),
		RawQuery: query,
	}
	return u.String()
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		var x int
		if _, err := fmt.Sscanf(v, "%d", &x); err == nil && x > 0 {
			return x
		}
	}
	return def
}
