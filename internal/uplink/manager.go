package uplink

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Request struct {
	CameraID       string `json:"cameraId"`
	ProxyPath      string `json:"proxyPath"`
	CentralHost    string `json:"centralHost"`
	CentralSrtPort int    `json:"centralSrtPort"`
	CentralPath    string `json:"centralPath"`
	TTLSeconds     int    `json:"ttlSeconds"`
}

func (r *Request) Normalize() {
	r.CameraID = strings.TrimSpace(r.CameraID)
	r.ProxyPath = strings.TrimSpace(r.ProxyPath)
	r.CentralHost = strings.TrimSpace(r.CentralHost)
	r.CentralPath = strings.TrimSpace(r.CentralPath)
}

func (r Request) Validate() error {
	if r.CameraID == "" {
		return fmt.Errorf("cameraId obrigatório")
	}
	if r.ProxyPath == "" {
		return fmt.Errorf("proxyPath obrigatório")
	}
	if r.CentralHost == "" {
		return fmt.Errorf("centralHost obrigatório")
	}
	if r.CentralSrtPort <= 0 {
		return fmt.Errorf("centralSrtPort inválido")
	}
	if r.CentralPath == "" {
		return fmt.Errorf("centralPath obrigatório")
	}
	if r.TTLSeconds <= 0 {
		return fmt.Errorf("ttlSeconds inválido")
	}
	return nil
}

type Manager struct {
	mu        sync.Mutex
	processes map[string]*processEntry
	command   string
}

type processEntry struct {
	cmd       *exec.Cmd
	watchers  int
	ttl       time.Duration
	stopTimer *time.Timer
}

func NewManagerFromEnv() *Manager {
	command := strings.TrimSpace(os.Getenv("UPLINK_COMMAND"))
	if command == "" {
		command = "ffmpeg"
	}
	return &Manager{
		processes: make(map[string]*processEntry),
		command:   command,
	}
}

func (m *Manager) Start(req Request) error {
	req.Normalize()
	if err := req.Validate(); err != nil {
		return err
	}

	inputURL := fmt.Sprintf("rtsp://mtx-proxy:8554/%s", strings.TrimPrefix(req.ProxyPath, "/"))
	outputURL := fmt.Sprintf(
		"srt://%s:%d?streamid=publish:%s",
		req.CentralHost,
		req.CentralSrtPort,
		strings.TrimPrefix(req.CentralPath, "/"),
	)
	ttl := time.Duration(req.TTLSeconds) * time.Second

	m.mu.Lock()
	if entry, ok := m.processes[req.CameraID]; ok {
		entry.watchers++
		entry.ttl = ttl
		if entry.stopTimer != nil {
			entry.stopTimer.Stop()
			entry.stopTimer = nil
		}
		m.mu.Unlock()
		log.Printf("[uplink] camera %s already running, watchers=%d", req.CameraID, entry.watchers)
		return nil
	}

	cmd := exec.Command(m.command,
		"-rtsp_transport", "tcp",
		"-i", inputURL,
		"-c", "copy",
		"-f", "mpegts",
		outputURL,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("start uplink command: %w", err)
	}

	entry := &processEntry{
		cmd:      cmd,
		watchers: 1,
		ttl:      ttl,
	}
	m.processes[req.CameraID] = entry
	m.mu.Unlock()

	log.Printf("[uplink] started camera %s -> %s", req.CameraID, outputURL)
	go m.waitForExit(req.CameraID, cmd)
	return nil
}

func (m *Manager) Stop(req Request) error {
	req.Normalize()
	if err := req.Validate(); err != nil {
		return err
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second

	m.mu.Lock()
	entry, ok := m.processes[req.CameraID]
	if !ok {
		m.mu.Unlock()
		log.Printf("[uplink] camera %s not running", req.CameraID)
		return nil
	}

	if entry.watchers > 0 {
		entry.watchers--
	}
	entry.ttl = ttl
	if entry.watchers > 0 {
		m.mu.Unlock()
		log.Printf("[uplink] camera %s still has watchers=%d", req.CameraID, entry.watchers)
		return nil
	}

	if entry.stopTimer != nil {
		entry.stopTimer.Stop()
		entry.stopTimer = nil
	}

	if ttl <= 0 {
		cmd := entry.cmd
		delete(m.processes, req.CameraID)
		m.mu.Unlock()
		m.stopCommand(req.CameraID, cmd, "stop")
		return nil
	}

	entry.stopTimer = time.AfterFunc(ttl, func() {
		m.expire(req.CameraID)
	})
	m.mu.Unlock()
	log.Printf("[uplink] camera %s scheduled to stop in %s", req.CameraID, ttl)
	return nil
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	entries := make(map[string]*processEntry, len(m.processes))
	for cameraID, entry := range m.processes {
		entries[cameraID] = entry
	}
	m.processes = make(map[string]*processEntry)
	m.mu.Unlock()

	for cameraID, entry := range entries {
		if entry.stopTimer != nil {
			entry.stopTimer.Stop()
		}
		m.stopCommand(cameraID, entry.cmd, "shutdown")
	}
}

func (m *Manager) expire(cameraID string) {
	var cmd *exec.Cmd

	m.mu.Lock()
	entry, ok := m.processes[cameraID]
	if !ok || entry.watchers > 0 {
		m.mu.Unlock()
		return
	}
	cmd = entry.cmd
	delete(m.processes, cameraID)
	m.mu.Unlock()

	m.stopCommand(cameraID, cmd, "ttl")
}

func (m *Manager) waitForExit(cameraID string, cmd *exec.Cmd) {
	err := cmd.Wait()
	if err != nil {
		log.Printf("[uplink] camera %s exited with error: %v", cameraID, err)
	} else {
		log.Printf("[uplink] camera %s exited", cameraID)
	}

	m.mu.Lock()
	entry, ok := m.processes[cameraID]
	if ok && entry.cmd == cmd {
		if entry.stopTimer != nil {
			entry.stopTimer.Stop()
		}
		delete(m.processes, cameraID)
	}
	m.mu.Unlock()
}

func (m *Manager) stopCommand(cameraID string, cmd *exec.Cmd, reason string) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil {
		log.Printf("[uplink] failed to stop camera %s (%s): %v", cameraID, reason, err)
		return
	}
	log.Printf("[uplink] stopped camera %s (%s)", cameraID, reason)
}
