package uplink

import (
	"fmt"
	"log"
	"net/url"
	"os"
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
	if strings.TrimSpace(host) == "" || strings.TrimSpace(path) == "" {
		log.Printf("[uplink] host/path inválidos para SRT (host=%q path=%q)", host, path)
		return nil
	}
	options := srtOptionsFromEnv()
	candidates := srtOptionCandidates(options)
	urls := make([]string, 0, len(candidates))
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		srtURL := buildSRTURLWithOptions(host, port, path, candidate)
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
	return urls
}

func buildSRTURLWithOptions(host string, port int, path string, opts SRTQueryOptions) string {
	if port <= 0 {
		port = defaultSRTPort
	}
	streamID := fmt.Sprintf("publish:%s", path)
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
	u := url.URL{
		Scheme:   "srt",
		Host:     fmt.Sprintf("%s:%d", host, port),
		RawQuery: queryValues.Encode(),
	}
	return u.String()
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
	if opts.ExtraParams == "" {
		return
	}
	parsed, err := url.ParseQuery(opts.ExtraParams)
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
