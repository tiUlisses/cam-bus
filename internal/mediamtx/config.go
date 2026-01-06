package mediamtx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sua-org/cam-bus/internal/core"
	"gopkg.in/yaml.v3"
)

const maxRecordDeleteAfter = 10 * time.Minute

// Config representa o YAML mínimo do MediaMTX para caminhos dinâmicos.
type Config struct {
	RTSPAddress       string                `yaml:"rtspAddress,omitempty"`
	HLS               bool                  `yaml:"hls"`
	WebRTC            bool                  `yaml:"webrtc"`
	API               bool                  `yaml:"api"`
	APIAddress        string                `yaml:"apiAddress,omitempty"`
	AuthInternalUsers []AuthInternalUser    `yaml:"authInternalUsers,omitempty"`
	PathDefaults      PathDefaults          `yaml:"pathDefaults"`
	Paths             map[string]PathConfig `yaml:"paths"`
}

type PathDefaults struct {
	Record                bool   `yaml:"record"`
	RecordPath            string `yaml:"recordPath"`
	RecordFormat          string `yaml:"recordFormat"`
	RecordPartDuration    string `yaml:"recordPartDuration"`
	RecordSegmentDuration string `yaml:"recordSegmentDuration"`
	RecordDeleteAfter     string `yaml:"recordDeleteAfter"`
}

type PathConfig struct {
	Source            string `yaml:"source,omitempty"`
	SourceOnDemand    bool   `yaml:"sourceOnDemand"`
	Record            *bool  `yaml:"record,omitempty"`
	RecordDeleteAfter string `yaml:"recordDeleteAfter,omitempty"`
}

type AuthInternalUser struct {
	User        string           `yaml:"user"`
	Pass        string           `yaml:"pass,omitempty"`
	IPs         []string         `yaml:"ips,omitempty"`
	Permissions []AuthPermission `yaml:"permissions,omitempty"`
}

type AuthPermission struct {
	Action string `yaml:"action"`
	Path   string `yaml:"path,omitempty"`
}

// Generator gera e aplica configs do MediaMTX a partir de câmeras ativas.
type Generator struct {
	path              string
	reloadPID         int
	reloadURL         string
	reloadAuthUser    string
	reloadAuthPass    string
	reloadAuthToken   string
	apiUser           string
	apiPass           string
	recordDeleteAfter time.Duration
	httpClient        *http.Client
	mu                sync.Mutex
}

// NewGeneratorFromEnv cria o gerador baseado em variáveis de ambiente.
// MTX_PROXY_CONFIG_PATH (obrigatório) define o destino do YAML.
// MTX_PROXY_RELOAD_PID ou MTX_PROXY_PID definem o PID para SIGHUP.
// MTX_PROXY_RELOAD_URL define um endpoint HTTP para reload.
// MTX_PROXY_RELOAD_USER/MTX_PROXY_RELOAD_PASS ou MTX_PROXY_RELOAD_TOKEN definem credenciais para reload HTTP.
// MTX_PROXY_API_USER/MTX_PROXY_API_PASS configuram authInternalUsers no YAML gerado.
// MTX_PROXY_API_TOKEN (legado) pode ser usado como fallback para o reload token.
// MTX_PROXY_RECORD_DELETE_AFTER (opcional) ajusta a retenção, limitada a 10m.
func NewGeneratorFromEnv() *Generator {
	path := strings.TrimSpace(os.Getenv("MTX_PROXY_CONFIG_PATH"))
	if path == "" {
		return nil
	}

	reloadURL := strings.TrimSpace(os.Getenv("MTX_PROXY_RELOAD_URL"))
	reloadPID := parsePIDEnv("MTX_PROXY_RELOAD_PID")
	if reloadPID == 0 {
		reloadPID = parsePIDEnv("MTX_PROXY_PID")
	}

	reloadUser := strings.TrimSpace(os.Getenv("MTX_PROXY_RELOAD_USER"))
	reloadPass := strings.TrimSpace(os.Getenv("MTX_PROXY_RELOAD_PASS"))
	reloadToken := strings.TrimSpace(os.Getenv("MTX_PROXY_RELOAD_TOKEN"))
	apiUser := strings.TrimSpace(os.Getenv("MTX_PROXY_API_USER"))
	apiPass := strings.TrimSpace(os.Getenv("MTX_PROXY_API_PASS"))
	apiToken := strings.TrimSpace(os.Getenv("MTX_PROXY_API_TOKEN"))
	if reloadUser == "" && reloadPass == "" && reloadToken == "" {
		// Fallback evita 401 quando authInternalUsers está habilitado no MediaMTX.
		reloadUser = apiUser
		reloadPass = apiPass
		reloadToken = apiToken
	}

	retention := parseDurationEnv("MTX_PROXY_RECORD_DELETE_AFTER", maxRecordDeleteAfter)
	if retention > maxRecordDeleteAfter {
		retention = maxRecordDeleteAfter
	}

	return &Generator{
		path:              path,
		reloadPID:         reloadPID,
		reloadURL:         reloadURL,
		reloadAuthUser:    reloadUser,
		reloadAuthPass:    reloadPass,
		reloadAuthToken:   reloadToken,
		apiUser:           apiUser,
		apiPass:           apiPass,
		recordDeleteAfter: retention,
		httpClient:        &http.Client{Timeout: 5 * time.Second},
	}
}

// Sync escreve a config e aplica reload quando necessário.
func (g *Generator) Sync(cameras []core.CameraInfo) error {
	if g == nil || g.path == "" {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	existing, exists, err := g.readExistingConfig()
	if err != nil {
		return err
	}
	cfg := buildConfig(cameras, g.recordDeleteAfter, g.apiUser, g.apiPass)
	if g.apiUser == "" && g.apiPass == "" {
		cfg.AuthInternalUsers = existing.AuthInternalUsers
	}
	if exists && reflect.DeepEqual(existing, cfg) {
		return nil
	}

	data, err := marshalConfig(cfg)
	if err != nil {
		return fmt.Errorf("marshal mediamtx config: %w", err)
	}
	if err := g.writeFile(data); err != nil {
		return err
	}

	if err := g.reload(); err != nil {
		return err
	}

	return nil
}

func buildConfig(cameras []core.CameraInfo, retention time.Duration, apiUser, apiPass string) Config {
	cfg := Config{
		RTSPAddress:       ":8554",
		HLS:               false,
		WebRTC:            false,
		API:               true,
		APIAddress:        ":9997",
		AuthInternalUsers: authUsersForAPI(apiUser, apiPass),
		PathDefaults: PathDefaults{
			Record:                true,
			RecordPath:            "/recordings/%path/%Y-%m-%d_%H-%M-%S-%f",
			RecordFormat:          "fmp4",
			RecordPartDuration:    "1s",
			RecordSegmentDuration: "1m",
			RecordDeleteAfter:     formatDuration(retention),
		},
		Paths: make(map[string]PathConfig),
	}

	for _, info := range cameras {
		path := strings.TrimSpace(info.ProxyPath)
		if path == "" {
			path = info.DeviceID
		}
		path = strings.TrimPrefix(path, "/")
		if path == "" {
			continue
		}

		rtspURL := strings.TrimSpace(info.RTSPURL)
		if rtspURL == "" {
			continue
		}

		cfg.Paths[path] = pathConfigFor(info, rtspURL, retention)
	}

	return cfg
}

func authUsersForAPI(apiUser, apiPass string) []AuthInternalUser {
	if apiUser == "" && apiPass == "" {
		return nil
	}
	return []AuthInternalUser{
		{
			User: "any",
			IPs:  []string{},
			Permissions: []AuthPermission{
				{Action: "publish"},
				{Action: "read"},
				{Action: "playback"},
			},
		},
		{
			User: apiUser,
			Pass: apiPass,
			IPs:  []string{},
			Permissions: []AuthPermission{
				{Action: "api"},
			},
		},
	}
}

func pathConfigFor(info core.CameraInfo, rtspURL string, defaultRetention time.Duration) PathConfig {
	cfg := PathConfig{
		Source:         rtspURL,
		SourceOnDemand: false,
	}

	if !info.RecordEnabled {
		disabled := false
		cfg.Record = &disabled
		return cfg
	}

	perCameraRetention := retentionForCamera(info, defaultRetention)
	if perCameraRetention != defaultRetention {
		cfg.RecordDeleteAfter = formatDuration(perCameraRetention)
	}

	return cfg
}

func retentionForCamera(info core.CameraInfo, defaultRetention time.Duration) time.Duration {
	if info.RecordRetentionMinutes <= 0 {
		return defaultRetention
	}
	retention := time.Duration(info.RecordRetentionMinutes) * time.Minute
	if retention > defaultRetention {
		return defaultRetention
	}
	return retention
}

func (g *Generator) readExistingConfig() (Config, bool, error) {
	data, err := os.ReadFile(g.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, false, nil
		}
		return Config{}, false, fmt.Errorf("read mediamtx config: %w", err)
	}

	var existing Config
	if err := yaml.Unmarshal(data, &existing); err != nil {
		return Config{}, false, nil
	}

	return existing, true, nil
}

func marshalConfig(cfg Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (g *Generator) writeFile(data []byte) error {
	dir := filepath.Dir(g.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(g.path, data, 0o644); err != nil {
		return fmt.Errorf("write mediamtx config: %w", err)
	}
	return nil
}

func (g *Generator) reload() error {
	if g.reloadURL != "" {
		return g.reloadViaHTTP()
	}
	if g.reloadPID > 0 {
		return g.reloadViaSignal()
	}
	return errors.New("mediamtx reload not configured")
}

func (g *Generator) reloadViaSignal() error {
	proc, err := os.FindProcess(g.reloadPID)
	if err != nil {
		return fmt.Errorf("find mediamtx process: %w", err)
	}
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("signal mediamtx reload: %w", err)
	}
	return nil
}

func (g *Generator) reloadViaHTTP() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.reloadURL, nil)
	if err != nil {
		return fmt.Errorf("create reload request: %w", err)
	}
	g.applyReloadAuth(req)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("reload mediamtx via HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("reload mediamtx via HTTP: status %s", resp.Status)
	}
	return nil
}

func (g *Generator) applyReloadAuth(req *http.Request) {
	if g.reloadAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+g.reloadAuthToken)
		return
	}
	if g.reloadAuthUser != "" || g.reloadAuthPass != "" {
		req.SetBasicAuth(g.reloadAuthUser, g.reloadAuthPass)
	}
}

func parseDurationEnv(key string, def time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return def
	}

	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		log.Printf("[mediamtx] duração inválida em %s=%q, usando default %s", key, value, def)
		return def
	}
	return duration
}

func parsePIDEnv(key string) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		log.Printf("[mediamtx] PID inválido em %s=%q", key, value)
		return 0
	}
	return pid
}

func formatDuration(duration time.Duration) string {
	if duration%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	}
	if duration%time.Second == 0 {
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	}
	return duration.String()
}
