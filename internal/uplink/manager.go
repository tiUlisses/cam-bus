package uplink

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sua-org/cam-bus/internal/core"
	"github.com/sua-org/cam-bus/internal/uplink/container"
)

const (
	defaultProxyRTSPBase = "rtsp://localhost:8554"
	defaultSRTPacketSize = 1316
	defaultSRTPort       = 8890
	defaultReconcileSecs = 15
)

type Manager struct {
	proxyRTSPBase      string
	defaultCentralHost string
	defaultSRTPort     int
	containerManager   *container.Manager
	reconcileInterval  time.Duration
	reconcileStop      chan struct{}
	mu                 sync.Mutex
	uplinks            map[string]*uplinkProcess
	statusHook         atomic.Value
}

type uplinkProcess struct {
	cameraKey       string
	payload         Request
	container       string
	containerID     string
	containerStatus string
	ttlTimer        *time.Timer
	// startCount increments for every Start request; stopCount increments for every Stop request.
	startCount int
	stopCount  int
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
	manager := &Manager{
		proxyRTSPBase:      strings.TrimSuffix(getenv("UPLINK_PROXY_RTSP_BASE", defaultProxyRTSPBase), "/"),
		defaultCentralHost: strings.TrimSpace(os.Getenv("UPLINK_CENTRAL_HOST")),
		defaultSRTPort:     getenvInt("UPLINK_CENTRAL_SRT_PORT", defaultSRTPort),
		containerManager:   container.NewManagerFromEnv(),
		reconcileInterval:  time.Duration(getenvInt("UPLINK_RECONCILE_INTERVAL_SECONDS", defaultReconcileSecs)) * time.Second,
		reconcileStop:      make(chan struct{}),
		uplinks:            make(map[string]*uplinkProcess),
	}
	manager.startReconciler()
	return manager
}

func (m *Manager) SetStatusHook(h StatusHook) {
	m.statusHook.Store(h)
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
			existing.startCount++
			log.Printf("[uplink] already running for %s, startCount=%d stopCount=%d, refreshing TTL", cameraKey, existing.startCount, existing.stopCount)
			m.refreshTTL(existing, req.TTLSeconds)
			return nil
		}
		m.stopProcess(existing, "restarting with new payload")
	}

	proxyURL := fmt.Sprintf("%s/%s", m.proxyRTSPBase, strings.TrimPrefix(req.ProxyPath, "/"))
	srtURL := buildSRTURL(req.CentralHost, req.CentralSRTPort, req.CentralPath)
	containerName := container.NameForCentralPath(req.CentralPath)
	startCtx := context.Background()
	containerID, err := m.containerManager.Start(startCtx, container.Request{
		Name:     containerName,
		ProxyURL: proxyURL,
		SRTURL:   srtURL,
	})
	if err != nil {
		log.Printf("[uplink] docker run failed for %s (container=%s): %v", cameraKey, containerName, err)
		m.notifyStatus(Status{
			CameraID:      req.CameraID,
			CentralPath:   req.CentralPath,
			ContainerName: containerName,
			State:         "error",
			ExitCode:      0,
			Error:         err.Error(),
		})
		return fmt.Errorf("start container uplink: %w", err)
	}

	proc := &uplinkProcess{
		cameraKey:       cameraKey,
		payload:         req,
		container:       containerName,
		containerID:     containerID,
		containerStatus: "running",
		startCount:      1,
		stopCount:       0,
	}
	m.uplinks[cameraKey] = proc
	m.refreshTTL(proc, req.TTLSeconds)

	log.Printf("[uplink] started for %s -> %s (startCount=%d stopCount=%d)", cameraKey, srtURL, proc.startCount, proc.stopCount)
	m.notifyStatus(Status{
		CameraID:      req.CameraID,
		CentralPath:   req.CentralPath,
		ContainerName: containerName,
		State:         "running",
		ExitCode:      0,
		Error:         "",
	})
	return nil
}

func (m *Manager) startReconciler() {
	if m.reconcileInterval <= 0 {
		return
	}
	log.Printf("[uplink] reconcile loop started (interval=%s)", m.reconcileInterval)
	go func() {
		ticker := time.NewTicker(m.reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.reconcileOnce()
			case <-m.reconcileStop:
				return
			}
		}
	}()
}

type uplinkSnapshot struct {
	cameraKey   string
	cameraID    string
	centralPath string
	container   string
	containerID string
}

func (m *Manager) snapshotUplinks() []uplinkSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	snapshots := make([]uplinkSnapshot, 0, len(m.uplinks))
	for _, proc := range m.uplinks {
		snapshots = append(snapshots, uplinkSnapshot{
			cameraKey:   proc.cameraKey,
			cameraID:    proc.payload.CameraID,
			centralPath: proc.payload.CentralPath,
			container:   proc.container,
			containerID: proc.containerID,
		})
	}
	return snapshots
}

func (m *Manager) reconcileOnce() {
	snapshots := m.snapshotUplinks()
	if len(snapshots) == 0 {
		return
	}
	for _, snap := range snapshots {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		status, err := m.containerManager.InspectStatus(ctx, snap.container)
		cancel()
		if err != nil {
			log.Printf("[uplink] reconcile inspect failed for %s (container=%s): %v", snap.cameraKey, snap.container, err)
			continue
		}
		stateErr := strings.TrimSpace(status.Error)
		log.Printf("[uplink] reconcile status for %s container=%s state=%s exitCode=%d stateError=%s", snap.cameraKey, snap.container, status.State, status.ExitCode, stateErr)
		m.notifyStatus(Status{
			CameraID:      snap.cameraID,
			CentralPath:   snap.centralPath,
			ContainerName: snap.container,
			State:         status.State,
			ExitCode:      status.ExitCode,
			Error:         stateErr,
		})
		if status.State == "running" {
			m.mu.Lock()
			if proc, ok := m.uplinks[snap.cameraKey]; ok && proc.container == snap.container {
				proc.containerStatus = status.State
			}
			m.mu.Unlock()
			continue
		}
		m.mu.Lock()
		proc, ok := m.uplinks[snap.cameraKey]
		if ok && proc.container == snap.container {
			proc.containerStatus = status.State
			m.stopProcess(proc, fmt.Sprintf("container state=%s exitCode=%d stateError=%s", status.State, status.ExitCode, stateErr))
			delete(m.uplinks, snap.cameraKey)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) stopUplink(cameraKey, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	proc, ok := m.uplinks[cameraKey]
	if !ok {
		return fmt.Errorf("uplink not running")
	}
	proc.stopCount++
	if proc.stopCount >= proc.startCount {
		m.stopProcess(proc, reason)
		delete(m.uplinks, cameraKey)
		return nil
	}
	log.Printf("[uplink] stop requested for %s: %s (startCount=%d stopCount=%d), keeping uplink active", proc.cameraKey, reason, proc.startCount, proc.stopCount)
	return nil
}

func (m *Manager) stopProcess(proc *uplinkProcess, reason string) {
	if proc.ttlTimer != nil {
		proc.ttlTimer.Stop()
	}
	log.Printf("[uplink] stopping %s: %s (startCount=%d stopCount=%d)", proc.cameraKey, reason, proc.startCount, proc.stopCount)
	stopCtx := context.Background()
	if err := m.containerManager.Stop(stopCtx, proc.container); err != nil {
		log.Printf("[uplink] stopProcess failed for %s: %v", proc.cameraKey, err)
		m.notifyStatus(Status{
			CameraID:      proc.payload.CameraID,
			CentralPath:   proc.payload.CentralPath,
			ContainerName: proc.container,
			State:         "error",
			ExitCode:      0,
			Error:         err.Error(),
		})
		return
	}
	m.notifyStatus(Status{
		CameraID:      proc.payload.CameraID,
		CentralPath:   proc.payload.CentralPath,
		ContainerName: proc.container,
		State:         "stopped",
		ExitCode:      0,
		Error:         reason,
	})
}

func (m *Manager) notifyStatus(status Status) {
	if status.Timestamp.IsZero() {
		status.Timestamp = time.Now().UTC()
	} else {
		status.Timestamp = status.Timestamp.UTC()
	}
	hookValue := m.statusHook.Load()
	hook, ok := hookValue.(StatusHook)
	if !ok || hook == nil {
		return
	}
	hook(status)
}

func (m *Manager) refreshTTL(proc *uplinkProcess, ttlSeconds int) {
	if proc.ttlTimer != nil {
		proc.ttlTimer.Stop()
		proc.ttlTimer = nil
	}
	if ttlSeconds <= 0 {
		return
	}
	log.Printf("[uplink] refreshing ttl for %s (ttlSeconds=%d startCount=%d stopCount=%d)", proc.cameraKey, ttlSeconds, proc.startCount, proc.stopCount)
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
