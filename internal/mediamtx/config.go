package mediamtx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sua-org/cam-bus/internal/core"
	"github.com/sua-org/cam-bus/internal/uplink"
	"gopkg.in/yaml.v3"
)

const (
	maxRecordDeleteAfter = 10 * time.Minute
	defaultProxyRTSPBase = "rtsp://localhost:8554"
)

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
	Record                bool   `yaml:"record" json:"record"`
	RecordPath            string `yaml:"recordPath" json:"recordPath"`
	RecordFormat          string `yaml:"recordFormat" json:"recordFormat"`
	RecordPartDuration    string `yaml:"recordPartDuration" json:"recordPartDuration"`
	RecordSegmentDuration string `yaml:"recordSegmentDuration" json:"recordSegmentDuration"`
	RecordDeleteAfter     string `yaml:"recordDeleteAfter" json:"recordDeleteAfter"`
}

type PathConfig struct {
	Source            string `yaml:"source,omitempty" json:"source,omitempty"`
	SourceOnDemand    bool   `yaml:"sourceOnDemand" json:"sourceOnDemand"`
	RunOnReady        string `yaml:"runOnReady,omitempty" json:"runOnReady,omitempty"`
	RunOnReadyRestart bool   `yaml:"runOnReadyRestart,omitempty" json:"runOnReadyRestart,omitempty"`
	Record            *bool  `yaml:"record,omitempty" json:"record,omitempty"`
	RecordDeleteAfter string `yaml:"recordDeleteAfter,omitempty" json:"recordDeleteAfter,omitempty"`
}

type AuthInternalUser struct {
	User        string           `yaml:"user" json:"user"`
	Pass        string           `yaml:"pass,omitempty" json:"pass,omitempty"`
	IPs         []string         `yaml:"ips,omitempty" json:"ips,omitempty"`
	Permissions []AuthPermission `yaml:"permissions,omitempty" json:"permissions,omitempty"`
}

type AuthPermission struct {
	Action string `yaml:"action" json:"action"`
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`
}

type GlobalPatch struct {
	RTSPAddress       string             `json:"rtspAddress"`
	HLS               bool               `json:"hls"`
	WebRTC            bool               `json:"webrtc"`
	API               bool               `json:"api"`
	APIAddress        string             `json:"apiAddress"`
	AuthInternalUsers []AuthInternalUser `json:"authInternalUsers"`
}

// Generator gera e aplica configs do MediaMTX a partir de câmeras ativas.
type Generator struct {
	path               string
	reloadPID          int
	apiBaseURL         string
	reloadAuthUser     string
	reloadAuthPass     string
	reloadAuthToken    string
	apiUser            string
	apiPass            string
	recordDeleteAfter  time.Duration
	republishOnReady   bool
	proxyRTSPBase      string
	httpClient         *http.Client
	ignoreUplink       bool
	defaultCentralHost string
	useCentralPaths    bool
	sourceFromProxy    bool
	preserveDefaults   bool
	mu                 sync.Mutex
}

// NewGeneratorFromEnv cria o gerador baseado em variáveis de ambiente.
// MTX_PROXY_CONFIG_PATH (obrigatório) define o destino do YAML.
// MTX_PROXY_RELOAD_PID ou MTX_PROXY_PID definem o PID para SIGHUP.
// MTX_PROXY_RELOAD_URL define a base HTTP da API do MediaMTX (ex.: http://mtx-proxy:9997).
// MTX_PROXY_RELOAD_USER/MTX_PROXY_RELOAD_PASS ou MTX_PROXY_RELOAD_TOKEN definem credenciais para reload HTTP.
// MTX_PROXY_API_USER/MTX_PROXY_API_PASS configuram authInternalUsers no YAML gerado.
// MTX_PROXY_API_TOKEN (legado) pode ser usado como fallback para o reload token.
// MTX_PROXY_RECORD_DELETE_AFTER (opcional) ajusta a retenção, limitada a 10m.
func NewGeneratorFromEnv() *Generator {
	path := strings.TrimSpace(os.Getenv("MTX_PROXY_CONFIG_PATH"))
	if path == "" {
		return nil
	}

	apiBaseURL := normalizeAPIBaseURL(os.Getenv("MTX_PROXY_RELOAD_URL"))
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

	uplinkMode := strings.ToLower(strings.TrimSpace(os.Getenv("UPLINK_MODE")))
	ignoreUplink := getenvBool("IGNORE_UPLINK", false)
	republishOnReady := uplinkMode == "mediamtx" || ignoreUplink
	proxyRTSPBase := strings.TrimSuffix(getenv("UPLINK_PROXY_RTSP_BASE", defaultProxyRTSPBase), "/")
	defaultCentralHost := strings.TrimSpace(os.Getenv("UPLINK_CENTRAL_HOST"))

	return &Generator{
		path:               path,
		reloadPID:          reloadPID,
		apiBaseURL:         apiBaseURL,
		reloadAuthUser:     reloadUser,
		reloadAuthPass:     reloadPass,
		reloadAuthToken:    reloadToken,
		apiUser:            apiUser,
		apiPass:            apiPass,
		recordDeleteAfter:  retention,
		republishOnReady:   republishOnReady,
		proxyRTSPBase:      proxyRTSPBase,
		httpClient:         &http.Client{Timeout: 5 * time.Second},
		ignoreUplink:       ignoreUplink,
		defaultCentralHost: defaultCentralHost,
	}
}

// NewCentralGeneratorFromEnv cria o gerador do MediaMTX central para RTSP pull via proxy.
// MTX_CENTRAL_CONFIG_PATH (obrigatório) define o destino do YAML.
// MTX_CENTRAL_RELOAD_PID ou MTX_CENTRAL_PID definem o PID para SIGHUP.
// MTX_CENTRAL_RELOAD_URL define a base HTTP da API do MediaMTX (ex.: http://mtx-central:9997).
// MTX_CENTRAL_RELOAD_USER/MTX_CENTRAL_RELOAD_PASS ou MTX_CENTRAL_RELOAD_TOKEN definem credenciais para reload HTTP.
// MTX_CENTRAL_API_USER/MTX_CENTRAL_API_PASS configuram authInternalUsers no YAML gerado.
// MTX_CENTRAL_API_TOKEN (legado) pode ser usado como fallback para o reload token.
// MTX_CENTRAL_RECORD_DELETE_AFTER (opcional) ajusta a retenção, limitada a 10m.
func NewCentralGeneratorFromEnv() *Generator {
	path := strings.TrimSpace(os.Getenv("MTX_CENTRAL_CONFIG_PATH"))
	if path == "" {
		return nil
	}

	apiBaseURL := normalizeAPIBaseURL(os.Getenv("MTX_CENTRAL_RELOAD_URL"))
	reloadPID := parsePIDEnv("MTX_CENTRAL_RELOAD_PID")
	if reloadPID == 0 {
		reloadPID = parsePIDEnv("MTX_CENTRAL_PID")
	}

	reloadUser := strings.TrimSpace(os.Getenv("MTX_CENTRAL_RELOAD_USER"))
	reloadPass := strings.TrimSpace(os.Getenv("MTX_CENTRAL_RELOAD_PASS"))
	reloadToken := strings.TrimSpace(os.Getenv("MTX_CENTRAL_RELOAD_TOKEN"))
	apiUser := strings.TrimSpace(os.Getenv("MTX_CENTRAL_API_USER"))
	apiPass := strings.TrimSpace(os.Getenv("MTX_CENTRAL_API_PASS"))
	apiToken := strings.TrimSpace(os.Getenv("MTX_CENTRAL_API_TOKEN"))
	if reloadUser == "" && reloadPass == "" && reloadToken == "" {
		reloadUser = apiUser
		reloadPass = apiPass
		reloadToken = apiToken
	}

	retention := parseDurationEnv("MTX_CENTRAL_RECORD_DELETE_AFTER", maxRecordDeleteAfter)
	if retention > maxRecordDeleteAfter {
		retention = maxRecordDeleteAfter
	}

	ignoreUplink := getenvBool("IGNORE_UPLINK", false)
	proxyRTSPBase := strings.TrimSuffix(getenv("UPLINK_PROXY_RTSP_BASE", defaultProxyRTSPBase), "/")
	defaultCentralHost := strings.TrimSpace(os.Getenv("UPLINK_CENTRAL_HOST"))

	return &Generator{
		path:               path,
		reloadPID:          reloadPID,
		apiBaseURL:         apiBaseURL,
		reloadAuthUser:     reloadUser,
		reloadAuthPass:     reloadPass,
		reloadAuthToken:    reloadToken,
		apiUser:            apiUser,
		apiPass:            apiPass,
		recordDeleteAfter:  retention,
		republishOnReady:   false,
		proxyRTSPBase:      proxyRTSPBase,
		httpClient:         &http.Client{Timeout: 5 * time.Second},
		ignoreUplink:       ignoreUplink,
		defaultCentralHost: defaultCentralHost,
		useCentralPaths:    true,
		sourceFromProxy:    true,
		preserveDefaults:   true,
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
	cfg := g.buildConfig(existing, exists, cameras)
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

	if err := g.applyChanges(existing, cfg); err != nil {
		return err
	}

	return nil
}

func baseConfig(retention time.Duration, apiUser, apiPass string) Config {
	return Config{
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
}

func (g *Generator) buildConfig(existing Config, exists bool, cameras []core.CameraInfo) Config {
	var cfg Config
	if g.preserveDefaults && exists {
		cfg = existing
		cfg.Paths = make(map[string]PathConfig)
		if g.apiUser != "" || g.apiPass != "" {
			cfg.AuthInternalUsers = authUsersForAPI(g.apiUser, g.apiPass)
		}
		if cfg.PathDefaults.RecordDeleteAfter == "" {
			cfg.PathDefaults.RecordDeleteAfter = formatDuration(g.recordDeleteAfter)
		}
	} else {
		cfg = baseConfig(g.recordDeleteAfter, g.apiUser, g.apiPass)
		if exists && g.apiUser == "" && g.apiPass == "" {
			cfg.AuthInternalUsers = existing.AuthInternalUsers
		}
	}
	for _, info := range cameras {
		if g.ignoreUplink {
			if info.CentralHost == "" {
				info.CentralHost = g.defaultCentralHost
			}
			if info.CentralPath == "" {
				info.CentralPath = uplink.CentralPathFor(info)
			}
		}
		path := g.pathNameFor(info)
		if path == "" {
			continue
		}

		rtspURL := g.sourceURLFor(info)
		if rtspURL == "" && !g.republishOnReady {
			continue
		}

		cfg.Paths[path] = pathConfigFor(info, rtspURL, g.recordDeleteAfter, g.republishOnReady, g.proxyRTSPBase)
	}

	return cfg
}

func (g *Generator) pathNameFor(info core.CameraInfo) string {
	var path string
	if g.useCentralPaths {
		path = strings.TrimSpace(info.CentralPath)
		if path == "" {
			path = uplink.CentralPathFor(info)
		}
	} else {
		path = strings.TrimSpace(info.ProxyPath)
		if path == "" {
			path = info.DeviceID
		}
	}
	path = strings.TrimPrefix(path, "/")
	if path == "." {
		return ""
	}
	return strings.TrimSpace(path)
}

func (g *Generator) sourceURLFor(info core.CameraInfo) string {
	if g.sourceFromProxy {
		proxyPath := strings.TrimSpace(info.ProxyPath)
		if proxyPath == "" {
			proxyPath = info.DeviceID
		}
		proxyPath = strings.TrimPrefix(proxyPath, "/")
		if proxyPath == "" {
			return ""
		}
		return fmt.Sprintf("%s/%s", strings.TrimSuffix(g.proxyRTSPBase, "/"), proxyPath)
	}
	return strings.TrimSpace(info.RTSPURL)
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

func pathConfigFor(info core.CameraInfo, rtspURL string, defaultRetention time.Duration, republishOnReady bool, proxyRTSPBase string) PathConfig {
	cfg := PathConfig{
		Source:         rtspURL,
		SourceOnDemand: false,
	}

	if republishOnReady && info.CentralHost != "" && info.CentralPath != "" {
		if cmd := buildRepublishCommand(proxyRTSPBase, info); cmd != "" {
			cfg.RunOnReady = cmd
			cfg.RunOnReadyRestart = true
		}
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

func buildRepublishCommand(proxyRTSPBase string, info core.CameraInfo) string {
	proxyPath := strings.Trim(strings.TrimSpace(info.ProxyPath), "/")
	if proxyPath == "" {
		proxyPath = strings.Trim(strings.TrimSpace(info.DeviceID), "/")
	}
	proxyURL := fmt.Sprintf("%s/%s", strings.TrimSuffix(proxyRTSPBase, "/"), proxyPath)
	srtURLs, err := uplink.BuildSRTURLCandidates(info.CentralHost, info.CentralSRTPort, info.CentralPath)
	if err != nil {
		log.Printf("[mediamtx] srt candidates indisponíveis host=%q path=%q err=%v", info.CentralHost, info.CentralPath, err)
		return ""
	}
	if len(srtURLs) == 0 {
		log.Printf("[mediamtx] srt candidates vazios host=%q path=%q", info.CentralHost, info.CentralPath)
		return ""
	}

	args := []string{"/usr/local/bin/republish-srt", "--proxy-url", proxyURL, "--"}
	args = append(args, srtURLs...)
	return joinCommandArgs(args)
}

func joinCommandArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	escaped := strings.ReplaceAll(value, "'", `'"'"'`)
	return "'" + escaped + "'"
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

func (g *Generator) applyChanges(existing, desired Config) error {
	if g.apiBaseURL != "" {
		return g.applyConfigViaAPI(existing, desired)
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

func (g *Generator) applyConfigViaAPI(existing, desired Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	globalPatch := GlobalPatch{
		RTSPAddress:       desired.RTSPAddress,
		HLS:               desired.HLS,
		WebRTC:            desired.WebRTC,
		API:               desired.API,
		APIAddress:        desired.APIAddress,
		AuthInternalUsers: desired.AuthInternalUsers,
	}

	if err := g.doJSON(ctx, http.MethodPatch, "v3/config/global/patch", globalPatch); err != nil {
		return fmt.Errorf("patch mediamtx global config: %w", err)
	}
	if err := g.doJSON(ctx, http.MethodPatch, "v3/config/pathdefaults/patch", desired.PathDefaults); err != nil {
		return fmt.Errorf("patch mediamtx path defaults: %w", err)
	}

	for name := range existing.Paths {
		if _, ok := desired.Paths[name]; !ok {
			endpoint := fmt.Sprintf("v3/config/paths/delete/%s", url.PathEscape(name))
			if err := g.doJSON(ctx, http.MethodDelete, endpoint, nil); err != nil {
				return fmt.Errorf("delete mediamtx path %q: %w", name, err)
			}
		}
	}

	for name, pathCfg := range desired.Paths {
		endpoint := fmt.Sprintf("v3/config/paths/replace/%s", url.PathEscape(name))
		method := http.MethodPost
		if _, ok := existing.Paths[name]; !ok {
			endpoint = fmt.Sprintf("v3/config/paths/add/%s", url.PathEscape(name))
		}
		if err := g.doJSON(ctx, method, endpoint, pathCfg); err != nil {
			return fmt.Errorf("apply mediamtx path %q: %w", name, err)
		}
	}

	return nil
}

func (g *Generator) applyAPIAuth(req *http.Request) {
	if g.reloadAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+g.reloadAuthToken)
		return
	}
	if g.reloadAuthUser != "" || g.reloadAuthPass != "" {
		req.SetBasicAuth(g.reloadAuthUser, g.reloadAuthPass)
	}
}

func (g *Generator) doJSON(ctx context.Context, method, path string, payload any) error {
	var body *bytes.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	} else {
		body = bytes.NewReader(nil)
	}

	endpoint, err := g.buildAPIURL(path)
	if err != nil {
		return fmt.Errorf("build api url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	g.applyAPIAuth(req)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request mediamtx api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mediamtx api status %s", resp.Status)
	}
	return nil
}

func (g *Generator) buildAPIURL(path string) (string, error) {
	base, err := url.Parse(g.apiBaseURL)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	rel, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(rel).String(), nil
}

func normalizeAPIBaseURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	u, err := url.Parse(value)
	if err != nil {
		return strings.TrimRight(value, "/")
	}
	path := strings.TrimSuffix(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/v3/reload"):
		path = strings.TrimSuffix(path, "/v3/reload")
	case strings.HasSuffix(path, "/v3"):
		path = strings.TrimSuffix(path, "/v3")
	}
	u.Path = path
	return strings.TrimRight(u.String(), "/")
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
