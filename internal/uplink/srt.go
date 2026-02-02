package uplink

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type SRTQueryOptions struct {
	Latency     int
	PacketSize  int
	MaxBW       int
	RcvBuf      int
	Passphrase  string
	PBKeyLen    int
	PeerLatency int
	RcvLatency  int
	ConnTimeout int
	SndBuf      int
	InputBW     int
	OheadBW     int
	TLPktDrop   bool
	ExtraParams string
}

const (
	srtProfileCustom   = "custom"
	srtProfileLatency  = "latency"
	srtProfileBalanced = "balanced"
	srtProfileQuality  = "quality"
)

func BuildSRTURLCandidates(host string, port int, path string) []string {
	normalizedHost, normalizedPort, normalizedPath, err := normalizeSRTInputs(host, port, path)
	if err != nil {
		log.Printf("[uplink] host/path inválidos para SRT (host=%q path=%q): %v", host, path, err)
		return nil
	}
	options := srtOptionsFromEnv()
	candidates := srtOptionCandidates(options)
	urls := make([]string, 0, len(candidates))
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		for _, srtURL := range buildSRTURLVariants(normalizedHost, normalizedPort, normalizedPath, candidate) {
			if err := validateSRTURL(srtURL); err != nil {
				log.Printf("[uplink] srt url inválida: %v", err)
				continue
			}
			if _, ok := seen[srtURL]; ok {
				continue
			}
			seen[srtURL] = struct{}{}
			urls = append(urls, srtURL)
		}
	}
	return urls
}

func buildSRTURLVariants(host string, port int, path string, opts SRTQueryOptions) []string {
	streamID := streamIDForPath(path)
	queryValues := buildSRTQueryValues(streamID, opts)
	urls := []string{buildSRTURL(host, port, queryValues.Encode())}
	if isSafeRawStreamID(streamID) {
		rawQuery := buildSRTQueryWithRawStreamID(queryValues, streamID)
		urls = append(urls, buildSRTURL(host, port, rawQuery))
	}
	return urls
}

func buildSRTQueryValues(streamID string, opts SRTQueryOptions) url.Values {
	queryValues := url.Values{}
	queryValues.Set("streamid", streamID)
	queryValues.Set("mode", "caller")
	queryValues.Set("transtype", "live")
	if opts.PacketSize > 0 {
		queryValues.Set("pkt_size", fmt.Sprintf("%d", opts.PacketSize))
	}
	if opts.Latency > 0 {
		queryValues.Set("latency", fmt.Sprintf("%d", opts.Latency))
	}
	if opts.MaxBW > 0 {
		queryValues.Set("maxbw", fmt.Sprintf("%d", opts.MaxBW))
	}
	if opts.RcvBuf > 0 {
		queryValues.Set("rcvbuf", fmt.Sprintf("%d", opts.RcvBuf))
	}
	applySRTQueryOptions(queryValues, opts)
	return queryValues
}

func buildSRTURL(host string, port int, rawQuery string) string {
	u := url.URL{
		Scheme:   "srt",
		Host:     fmt.Sprintf("%s:%d", host, port),
		RawQuery: rawQuery,
	}
	return u.String()
}

func buildSRTQueryWithRawStreamID(values url.Values, streamID string) string {
	clone := url.Values{}
	for key, vals := range values {
		copied := make([]string, len(vals))
		copy(copied, vals)
		clone[key] = copied
	}
	clone.Del("streamid")
	encoded := clone.Encode()
	if encoded == "" {
		return fmt.Sprintf("streamid=%s", streamID)
	}
	return fmt.Sprintf("%s&streamid=%s", encoded, streamID)
}

func srtOptionsFromEnv() SRTQueryOptions {
	profile := srtProfileFromEnv()
	if profile == srtProfileCustom {
		return srtOptionsFromCustomEnv()
	}
	opts, ok := srtOptionsForProfile(profile)
	if !ok {
		log.Printf("[uplink] perfil SRT inválido UPLINK_SRT_PROFILE=%q, usando custom", profile)
		return srtOptionsFromCustomEnv()
	}
	applySRTAuxEnv(&opts)
	return opts
}

func srtOptionsFromCustomEnv() SRTQueryOptions {
	return SRTQueryOptions{
		Latency:     getenvInt("UPLINK_SRT_LATENCY", 0),
		PacketSize:  getenvInt("UPLINK_SRT_PACKET_SIZE", 0),
		MaxBW:       getenvInt("UPLINK_SRT_MAXBW", 0),
		RcvBuf:      getenvInt("UPLINK_SRT_RCVBUF", 0),
		Passphrase:  strings.TrimSpace(os.Getenv("UPLINK_SRT_PASSPHRASE")),
		PBKeyLen:    getenvInt("UPLINK_SRT_PBKEYLEN", 0),
		PeerLatency: getenvInt("UPLINK_SRT_PEERLATENCY", 0),
		RcvLatency:  getenvInt("UPLINK_SRT_RCVLATENCY", 0),
		ConnTimeout: getenvInt("UPLINK_SRT_CONNTIMEO", 0),
		SndBuf:      getenvInt("UPLINK_SRT_SNDBUF", 0),
		InputBW:     getenvInt("UPLINK_SRT_INPUTBW", 0),
		OheadBW:     getenvInt("UPLINK_SRT_OHEADBW", 0),
		TLPktDrop:   getenvBool("UPLINK_SRT_TLPKTDROP", false),
		ExtraParams: strings.TrimSpace(os.Getenv("UPLINK_SRT_EXTRA_PARAMS")),
	}
}

func srtOptionsForProfile(profile string) (SRTQueryOptions, bool) {
	switch profile {
	case srtProfileLatency:
		return SRTQueryOptions{
			Latency:    80,
			PacketSize: defaultSRTPacketSize,
			RcvBuf:     2_097_152,
		}, true
	case srtProfileBalanced:
		return SRTQueryOptions{
			Latency:    defaultSRTLatencyMS,
			PacketSize: defaultSRTPacketSize,
		}, true
	case srtProfileQuality:
		return SRTQueryOptions{
			Latency:    400,
			PacketSize: defaultSRTPacketSize,
			MaxBW:      8_000_000,
			RcvBuf:     8_388_608,
		}, true
	default:
		return SRTQueryOptions{}, false
	}
}

func applySRTAuxEnv(opts *SRTQueryOptions) {
	opts.Passphrase = strings.TrimSpace(os.Getenv("UPLINK_SRT_PASSPHRASE"))
	opts.PBKeyLen = getenvInt("UPLINK_SRT_PBKEYLEN", 0)
}

func srtProfileFromEnv() string {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("UPLINK_SRT_PROFILE")))
	if value == "" {
		return srtProfileCustom
	}
	return value
}

func srtOptionCandidates(base SRTQueryOptions) []SRTQueryOptions {
	base = withSRTDefaults(base)
	candidates := []SRTQueryOptions{base}

	stripped := base
	stripped.PeerLatency = 0
	stripped.RcvLatency = 0
	stripped.TLPktDrop = false
	stripped.ExtraParams = ""
	if !srtOptionsEqual(stripped, base) {
		candidates = append(candidates, stripped)
	}

	defaultLatency := stripped
	defaultLatency.Latency = defaultSRTLatencyMS
	if !srtOptionsEqual(defaultLatency, stripped) {
		candidates = append(candidates, defaultLatency)
	}

	compatEnabled := getenvBool("UPLINK_SRT_COMPAT_PROFILE", false)
	if !compatEnabled {
		return candidates
	}

	compat := stripped
	compat.Latency = 80
	compat.PeerLatency = 500
	compat.RcvLatency = 500
	compat.TLPktDrop = true
	if !srtOptionsEqual(compat, base) && !srtOptionsEqual(compat, stripped) && !srtOptionsEqual(compat, defaultLatency) {
		candidates = append(candidates, compat)
	}

	return candidates
}

func withSRTDefaults(opts SRTQueryOptions) SRTQueryOptions {
	if opts.Latency == 0 {
		opts.Latency = defaultSRTLatencyMS
	}
	if opts.PacketSize == 0 {
		opts.PacketSize = defaultSRTPacketSize
	}
	return opts
}

func srtOptionsEqual(a, b SRTQueryOptions) bool {
	return a.Latency == b.Latency &&
		a.PacketSize == b.PacketSize &&
		a.MaxBW == b.MaxBW &&
		a.RcvBuf == b.RcvBuf &&
		a.Passphrase == b.Passphrase &&
		a.PBKeyLen == b.PBKeyLen &&
		a.PeerLatency == b.PeerLatency &&
		a.RcvLatency == b.RcvLatency &&
		a.ConnTimeout == b.ConnTimeout &&
		a.SndBuf == b.SndBuf &&
		a.InputBW == b.InputBW &&
		a.OheadBW == b.OheadBW &&
		a.TLPktDrop == b.TLPktDrop &&
		a.ExtraParams == b.ExtraParams
}

func applySRTQueryOptions(queryValues url.Values, opts SRTQueryOptions) {
	if opts.Passphrase != "" {
		queryValues.Set("passphrase", opts.Passphrase)
	}
	if opts.PBKeyLen > 0 {
		queryValues.Set("pbkeylen", fmt.Sprintf("%d", opts.PBKeyLen))
	}
	if opts.PeerLatency > 0 {
		queryValues.Set("peerlatency", fmt.Sprintf("%d", opts.PeerLatency))
	}
	if opts.RcvLatency > 0 {
		queryValues.Set("rcvlatency", fmt.Sprintf("%d", opts.RcvLatency))
	}
	if opts.ConnTimeout > 0 {
		queryValues.Set("conntimeo", fmt.Sprintf("%d", opts.ConnTimeout))
	}
	if opts.SndBuf > 0 {
		queryValues.Set("sndbuf", fmt.Sprintf("%d", opts.SndBuf))
	}
	if opts.InputBW > 0 {
		queryValues.Set("inputbw", fmt.Sprintf("%d", opts.InputBW))
	}
	if opts.OheadBW > 0 {
		queryValues.Set("oheadbw", fmt.Sprintf("%d", opts.OheadBW))
	}
	if opts.TLPktDrop {
		queryValues.Set("tlpktdrop", "1")
	}
	extraParams := strings.TrimSpace(opts.ExtraParams)
	if extraParams == "" {
		return
	}
	extraParams = strings.TrimLeft(extraParams, "?&")
	parsed, err := url.ParseQuery(extraParams)
	if err != nil {
		log.Printf("[uplink] parâmetros SRT inválidos em UPLINK_SRT_EXTRA_PARAMS=%q: %v", opts.ExtraParams, err)
		return
	}
	for key, values := range parsed {
		if len(values) == 0 {
			queryValues.Set(key, "")
			continue
		}
		queryValues.Set(key, values[len(values)-1])
	}
}

func validateSRTURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse srt url: %w", err)
	}
	if parsed.Scheme != "srt" {
		return fmt.Errorf("srt url precisa de esquema srt: %q", raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("srt url sem host: %q", raw)
	}
	q := parsed.Query()
	if strings.TrimSpace(q.Get("streamid")) == "" {
		return fmt.Errorf("srt url sem streamid: %q", raw)
	}
	return nil
}

func normalizeSRTInputs(host string, port int, path string) (string, int, string, error) {
	normalizedHost := strings.TrimSpace(host)
	normalizedPath := strings.Trim(strings.TrimSpace(path), "/")
	if normalizedHost == "" || normalizedPath == "" {
		return "", 0, "", fmt.Errorf("host/path vazios")
	}

	if strings.Contains(normalizedHost, "://") {
		parsed, err := url.Parse(normalizedHost)
		if err != nil {
			return "", 0, "", fmt.Errorf("host inválido: %w", err)
		}
		if parsed.Host != "" {
			normalizedHost = parsed.Host
		}
	}

	if idx := strings.Index(normalizedHost, "/"); idx >= 0 {
		log.Printf("[uplink] host com path extra em %q, usando apenas %q", normalizedHost, normalizedHost[:idx])
		normalizedHost = normalizedHost[:idx]
	}

	hostname, hostPort := splitHostPort(normalizedHost)
	if hostname != "" {
		normalizedHost = hostname
	}
	if port <= 0 && hostPort > 0 {
		port = hostPort
	}
	if port <= 0 {
		port = defaultSRTPort
	}
	normalizedHost = strings.TrimSpace(normalizedHost)
	if normalizedHost == "" {
		return "", 0, "", fmt.Errorf("host inválido após normalização")
	}
	return normalizedHost, port, normalizedPath, nil
}

func splitHostPort(host string) (string, int) {
	parsed, err := url.Parse("//" + host)
	if err != nil {
		return host, 0
	}
	target := parsed.Host
	if target == "" {
		target = parsed.Path
	}
	if target == "" {
		return host, 0
	}
	hostname, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return target, 0
	}
	parsedPort, err := parsePort(portStr)
	if err != nil {
		log.Printf("[uplink] porta inválida em host %q: %v", host, err)
		return hostname, 0
	}
	return hostname, parsedPort
}

func parsePort(port string) (int, error) {
	port = strings.TrimSpace(port)
	if port == "" {
		return 0, fmt.Errorf("porta vazia")
	}
	value, err := parseInt(port)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, fmt.Errorf("porta inválida %q", port)
	}
	return value, nil
}

func parseInt(value string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func streamIDForPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if strings.HasPrefix(trimmed, "publish:") {
		return trimmed
	}
	return fmt.Sprintf("publish:%s", trimmed)
}

func isSafeRawStreamID(streamID string) bool {
	for _, r := range streamID {
		switch {
		case r >= 'a' && r <= 'z':
			continue
		case r >= 'A' && r <= 'Z':
			continue
		case r >= '0' && r <= '9':
			continue
		case r == ':' || r == '/' || r == '-' || r == '_' || r == '.' || r == '~':
			continue
		default:
			return false
		}
	}
	return true
}
