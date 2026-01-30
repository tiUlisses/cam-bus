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
	defaultSRTLatencyMS  = 200
	defaultReconcileSecs = 15

	uplinkModeContainer = "container"
	uplinkModeMediaMTX  = "mediamtx"
)

type Manager struct {
	proxyRTSPBase      string
	defaultCentralHost string
	defaultSRTPort     int
	mode               string
	containerManager   *container.Manager
	reconcileInterval  time.Duration
	reconcileStop      chan struct{}
	alwaysOn           bool
	alwaysOnPaths      map[string]struct{}
	ignoreUplink       bool
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
	alwaysOn        bool
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
	alwaysOnPaths := parseListEnv(os.Getenv("UPLINK_ALWAYS_ON_PATHS"))
	alwaysOn := getenvBool("UPLINK_ALWAYS_ON", false)
	defaultCentralHost := strings.TrimSpace(os.Getenv("UPLINK_CENTRAL_HOST"))
	defaultSRTPort := getenvInt("UPLINK_CENTRAL_SRT_PORT", defaultSRTPort)
	if alwaysOn && defaultCentralHost == "" {
		centralHost, centralPort := parseCentralURL(os.Getenv("MEDIAMTX_CENTRAL_URL"))
		if centralHost != "" {
			defaultCentralHost = centralHost
		}
		if centralPort > 0 {
			defaultSRTPort = centralPort
		}
	}
	manager := &Manager{
		proxyRTSPBase:      strings.TrimSuffix(getenv("UPLINK_PROXY_RTSP_BASE", defaultProxyRTSPBase), "/"),
		defaultCentralHost: defaultCentralHost,
		defaultSRTPort:     defaultSRTPort,
		mode:               normalizeMode(os.Getenv("UPLINK_MODE")),
		containerManager:   container.NewManagerFromEnv(),
		reconcileInterval:  time.Duration(getenvInt("UPLINK_RECONCILE_INTERVAL_SECONDS", defaultReconcileSecs)) * time.Second,
		reconcileStop:      make(chan struct{}),
		alwaysOn:           alwaysOn,
		alwaysOnPaths:      alwaysOnPaths,
		ignoreUplink:       getenvBool("IGNORE_UPLINK", false),
		uplinks:            make(map[string]*uplinkProcess),
	}
	manager.startReconciler()
	return manager
}

func (m *Manager) SetStatusHook(h StatusHook) {
	m.statusHook.Store(h)
}

func (m *Manager) IgnoreUplinkEnabled() bool {
	if m == nil {
		return false
	}
	return m.ignoreUplink
}

func (m *Manager) DefaultCentralHost() string {
	if m == nil {
		return ""
	}
	return m.defaultCentralHost
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
	if m.isAlwaysOnRequest(req) {
		log.Printf("[uplink] stop ignored for %s (always-on)", cameraKey)
		return nil
	}
	return m.stopUplink(cameraKey, "stop command")
}

func (m *Manager) StopByCamera(info core.CameraInfo) {
	if m == nil || m.ignoreUplink || m.alwaysOn {
		return
	}
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
	if m == nil || m.ignoreUplink {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, proc := range m.uplinks {
		m.stopProcess(proc, "shutdown")
		delete(m.uplinks, key)
	}
}

func (m *Manager) AlwaysOnEnabled(info core.CameraInfo) bool {
	if m == nil {
		return false
	}
	if m.ignoreUplink {
		return true
	}
	if m.alwaysOn {
		return true
	}
	if len(m.alwaysOnPaths) == 0 {
		return false
	}
	candidates := []string{
		info.CentralPath,
		info.ProxyPath,
		info.DeviceID,
	}
	for _, raw := range candidates {
		if raw == "" {
			continue
		}
		key := normalizeAlwaysOnKey(raw)
		if _, ok := m.alwaysOnPaths[key]; ok {
			return true
		}
	}
	return false
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

func (m *Manager) ResolveRequest(req Request) Request {
	return m.applyDefaults(req)
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

	alwaysOn := m.isAlwaysOnRequest(req)
	if existing, ok := m.uplinks[cameraKey]; ok {
		if sameRequest(existing.payload, req) {
			existing.startCount++
			existing.alwaysOn = alwaysOn
			log.Printf("[uplink] already running for %s, startCount=%d stopCount=%d, refreshing TTL", cameraKey, existing.startCount, existing.stopCount)
			m.refreshTTL(existing, req.TTLSeconds)
			return nil
		}
		m.stopProcess(existing, "restarting with new payload")
	}

	proxyURL := fmt.Sprintf("%s/%s", m.proxyRTSPBase, strings.TrimPrefix(req.ProxyPath, "/"))
	srtURL := buildSRTURL(req.CentralHost, req.CentralSRTPort, req.CentralPath)
	containerName := container.NameForCentralPath(req.CentralPath)

	if m.mode == uplinkModeMediaMTX {
		proc := &uplinkProcess{
			cameraKey:       cameraKey,
			payload:         req,
			container:       "mediamtx-proxy",
			containerID:     "",
			containerStatus: "running",
			alwaysOn:        alwaysOn,
			startCount:      1,
			stopCount:       0,
		}
		m.uplinks[cameraKey] = proc
		m.refreshTTL(proc, req.TTLSeconds)

		log.Printf("[uplink] mediamtx mode active for %s -> %s (startCount=%d stopCount=%d)", cameraKey, srtURL, proc.startCount, proc.stopCount)
		m.notifyStatus(Status{
			CameraID:      req.CameraID,
			CentralPath:   req.CentralPath,
			ContainerName: proc.container,
			State:         "running",
			ExitCode:      0,
			Error:         "",
		})
		return nil
	}

	startCtx := context.Background()
	containerID, err := m.containerManager.Start(startCtx, container.Request{
		Name:     containerName,
		ProxyURL: proxyURL,
		SRTURL:   srtURL,
	})
	if err != nil {
		log.Printf("[uplink] docker run failed for %s (container=%s): %v", cameraKey, containerName, err)
		statusError := err.Error()
		var startErr *container.StartError
		if errors.As(err, &startErr) && startErr.Kind == container.StartErrorKindUnsupportedOption && startErr.Summary != "" {
			statusError = startErr.Summary
		}
		m.notifyStatus(Status{
			CameraID:      req.CameraID,
			CentralPath:   req.CentralPath,
			ContainerName: containerName,
			State:         "error",
			ExitCode:      0,
			Error:         statusError,
		})
		return fmt.Errorf("start container uplink: %w", err)
	}

	proc := &uplinkProcess{
		cameraKey:       cameraKey,
		payload:         req,
		container:       containerName,
		containerID:     containerID,
		containerStatus: "running",
		alwaysOn:        alwaysOn,
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
	if m.mode != uplinkModeContainer {
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

func (m *Manager) StatusFor(req Request) (Status, bool) {
	if m == nil {
		return Status{}, false
	}
	req = m.applyDefaults(req)

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, proc := range m.uplinks {
		if req.CentralPath != "" && proc.payload.CentralPath == req.CentralPath {
			return statusFromProcess(proc), true
		}
		if req.CameraID != "" && proc.payload.CameraID == req.CameraID {
			return statusFromProcess(proc), true
		}
	}
	return Status{}, false
}

func statusFromProcess(proc *uplinkProcess) Status {
	state := strings.TrimSpace(proc.containerStatus)
	if state == "" {
		state = "running"
	}
	return Status{
		CameraID:      proc.payload.CameraID,
		CentralPath:   proc.payload.CentralPath,
		ContainerName: proc.container,
		State:         state,
		ExitCode:      0,
		Error:         "",
		Timestamp:     time.Now().UTC(),
	}
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
	if m.mode != uplinkModeContainer {
		return
	}
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
	if m != nil && m.ignoreUplink {
		log.Printf("[uplink] ignoreUplink ativo, ignorando stop para %s (%s)", cameraKey, reason)
		return nil
	}
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
	if m.mode == uplinkModeMediaMTX {
		m.notifyStatus(Status{
			CameraID:      proc.payload.CameraID,
			CentralPath:   proc.payload.CentralPath,
			ContainerName: proc.container,
			State:         "stopped",
			ExitCode:      0,
			Error:         reason,
		})
		return
	}
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
	if m != nil && m.ignoreUplink {
		log.Printf("[uplink] ttl ignored for %s (ignore_uplink)", proc.cameraKey)
		return
	}
	if proc.alwaysOn {
		log.Printf("[uplink] ttl ignored for %s (always-on)", proc.cameraKey)
		return
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

func (m *Manager) isAlwaysOnRequest(req Request) bool {
	if m == nil {
		return false
	}
	if m.ignoreUplink {
		return true
	}
	if m.alwaysOn {
		return true
	}
	if len(m.alwaysOnPaths) == 0 {
		return false
	}
	candidates := []string{
		req.CentralPath,
		req.ProxyPath,
		req.CameraID,
	}
	for _, raw := range candidates {
		if raw == "" {
			continue
		}
		key := normalizeAlwaysOnKey(raw)
		if _, ok := m.alwaysOnPaths[key]; ok {
			return true
		}
	}
	return false
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
	latency := getenvInt("UPLINK_SRT_LATENCY", defaultSRTLatencyMS)
	packetSize := getenvInt("UPLINK_SRT_PACKET_SIZE", defaultSRTPacketSize)
	maxBW := getenvInt("UPLINK_SRT_MAXBW", 0)
	rcvBuf := getenvInt("UPLINK_SRT_RCVBUF", 0)
	queryValues := url.Values{}
	queryValues.Set("streamid", streamID)
	queryValues.Set("mode", "caller")
	queryValues.Set("transtype", "live")
	if packetSize > 0 {
		queryValues.Set("pkt_size", fmt.Sprintf("%d", packetSize))
	}
	if latency > 0 {
		queryValues.Set("latency", fmt.Sprintf("%d", latency))
	}
	if maxBW > 0 {
		queryValues.Set("maxbw", fmt.Sprintf("%d", maxBW))
	}
	if rcvBuf > 0 {
		queryValues.Set("rcvbuf", fmt.Sprintf("%d", rcvBuf))
	}

	u := url.URL{
		Scheme:   "srt",
		Host:     fmt.Sprintf("%s:%d", host, port),
		RawQuery: queryValues.Encode(),
	}
	return u.String()
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvBool(key string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}

func parseListEnv(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	}) {
		key := normalizeAlwaysOnKey(part)
		if key == "" {
			continue
		}
		out[key] = struct{}{}
	}
	return out
}

func parseCentralURL(raw string) (string, int) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0
	}
	parsed := raw
	if !strings.Contains(raw, "://") {
		parsed = "srt://" + raw
	}
	u, err := url.Parse(parsed)
	if err != nil {
		return "", 0
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return "", 0
	}
	if port := strings.TrimSpace(u.Port()); port != "" {
		var parsedPort int
		if _, err := fmt.Sscanf(port, "%d", &parsedPort); err == nil && parsedPort > 0 {
			return host, parsedPort
		}
	}
	return host, 0
}

func normalizeAlwaysOnKey(raw string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(raw), "/"))
}

func normalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case uplinkModeMediaMTX:
		return uplinkModeMediaMTX
	case uplinkModeContainer, "":
		return uplinkModeContainer
	default:
		log.Printf("[uplink] modo inválido UPLINK_MODE=%q, usando %s", raw, uplinkModeContainer)
		return uplinkModeContainer
	}
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
