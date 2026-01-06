package uplink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sua-org/cam-bus/internal/mqttclient"
)

const (
	defaultProxyRTSPBase = "rtsp://localhost:8554"
	defaultFFmpegBin     = "ffmpeg"
	defaultSRTPacketSize = 1316
	defaultSRTPort       = 8890
)

type Manager struct {
	mqtt          *mqttclient.Client
	baseTopic     string
	ffmpegBin     string
	proxyRTSPBase string
	mu            sync.Mutex
	uplinks       map[string]*uplinkProcess
}

type uplinkProcess struct {
	cameraKey string
	payload   StartPayload
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	ttlTimer  *time.Timer
}

type StartPayload struct {
	CameraID       string `json:"cameraId"`
	ProxyPath      string `json:"proxyPath"`
	CentralHost    string `json:"centralHost"`
	CentralSRTPort int    `json:"centralSrtPort"`
	CentralPath    string `json:"centralPath"`
	TTLSeconds     int    `json:"ttlSeconds"`
}

type StopPayload struct {
	CameraID    string `json:"cameraId"`
	CentralPath string `json:"centralPath"`
}

func NewManager(mqtt *mqttclient.Client, baseTopic string) *Manager {
	return &Manager{
		mqtt:          mqtt,
		baseTopic:     strings.TrimSuffix(baseTopic, "/"),
		ffmpegBin:     getenv("UPLINK_FFMPEG_BIN", defaultFFmpegBin),
		proxyRTSPBase: strings.TrimSuffix(getenv("UPLINK_PROXY_RTSP_BASE", defaultProxyRTSPBase), "/"),
		uplinks:       make(map[string]*uplinkProcess),
	}
}

func (m *Manager) Run(ctx context.Context) error {
	startTopic := fmt.Sprintf("%s/+/+/uplink/start", m.baseTopic)
	stopTopic := fmt.Sprintf("%s/+/+/uplink/stop", m.baseTopic)
	log.Printf("[uplink] subscribing to start topic: %s", startTopic)
	if err := m.mqtt.Subscribe(startTopic, 1, m.handleStart); err != nil {
		return fmt.Errorf("subscribe start uplink: %w", err)
	}
	log.Printf("[uplink] subscribing to stop topic: %s", stopTopic)
	if err := m.mqtt.Subscribe(stopTopic, 1, m.handleStop); err != nil {
		return fmt.Errorf("subscribe stop uplink: %w", err)
	}
	<-ctx.Done()
	log.Printf("[uplink] context canceled, stopping all uplinks")
	m.stopAll()
	return nil
}

func (m *Manager) handleStart(topic string, payload []byte) {
	tenant, building, err := m.extractScope(topic)
	if err != nil {
		log.Printf("[uplink] invalid start topic %s: %v", topic, err)
		return
	}
	var start StartPayload
	if err := json.Unmarshal(payload, &start); err != nil {
		log.Printf("[uplink] invalid start payload on %s: %v", topic, err)
		return
	}
	if err := validateStartPayload(start); err != nil {
		log.Printf("[uplink] invalid start payload on %s: %v", topic, err)
		return
	}
	cameraKey := fmt.Sprintf("%s|%s|%s", tenant, building, start.CameraID)
	if err := m.startUplink(cameraKey, start); err != nil {
		log.Printf("[uplink] start failed for %s: %v", cameraKey, err)
	}
}

func (m *Manager) handleStop(topic string, payload []byte) {
	tenant, building, err := m.extractScope(topic)
	if err != nil {
		log.Printf("[uplink] invalid stop topic %s: %v", topic, err)
		return
	}
	var stop StopPayload
	if err := json.Unmarshal(payload, &stop); err != nil {
		log.Printf("[uplink] invalid stop payload on %s: %v", topic, err)
		return
	}
	if stop.CameraID == "" {
		log.Printf("[uplink] invalid stop payload on %s: cameraId required", topic)
		return
	}
	cameraKey := fmt.Sprintf("%s|%s|%s", tenant, building, stop.CameraID)
	if err := m.stopUplink(cameraKey, "stop command"); err != nil {
		log.Printf("[uplink] stop failed for %s: %v", cameraKey, err)
	}
}

func (m *Manager) extractScope(topic string) (string, string, error) {
	parts := strings.Split(topic, "/")
	baseParts := strings.Split(m.baseTopic, "/")
	if len(parts) < len(baseParts)+3 {
		return "", "", fmt.Errorf("topic too short")
	}
	offset := len(baseParts)
	return parts[offset], parts[offset+1], nil
}

func (m *Manager) startUplink(cameraKey string, payload StartPayload) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.uplinks[cameraKey]; ok {
		if samePayload(existing.payload, payload) {
			log.Printf("[uplink] already running for %s, refreshing TTL", cameraKey)
			m.refreshTTL(existing, payload.TTLSeconds)
			return nil
		}
		m.stopProcess(existing, "restarting with new payload")
	}

	proxyURL := fmt.Sprintf("%s/%s", m.proxyRTSPBase, strings.TrimPrefix(payload.ProxyPath, "/"))
	srtURL := buildSRTURL(payload.CentralHost, payload.CentralSRTPort, payload.CentralPath)

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
		payload:   payload,
		cancel:    cancel,
		cmd:       cmd,
	}
	m.uplinks[cameraKey] = proc
	m.refreshTTL(proc, payload.TTLSeconds)

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

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, proc := range m.uplinks {
		m.stopProcess(proc, "shutdown")
		delete(m.uplinks, key)
	}
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

func validateStartPayload(payload StartPayload) error {
	if payload.CameraID == "" {
		return errors.New("cameraId required")
	}
	if payload.ProxyPath == "" {
		return errors.New("proxyPath required")
	}
	if payload.CentralHost == "" {
		return errors.New("centralHost required")
	}
	if payload.CentralPath == "" {
		return errors.New("centralPath required")
	}
	return nil
}

func samePayload(a, b StartPayload) bool {
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
	values := url.Values{}
	values.Set("streamid", fmt.Sprintf("publish:%s", path))
	values.Set("pkt_size", fmt.Sprintf("%d", defaultSRTPacketSize))

	u := url.URL{
		Scheme:   "srt",
		Host:     fmt.Sprintf("%s:%d", host, port),
		RawQuery: values.Encode(),
	}
	return u.String()
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
