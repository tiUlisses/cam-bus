// internal/core/types.go
package core

import "time"

type CameraInfo struct {
	IP           string   `json:"ip"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	Username     string   `json:"username"`
	Password     string   `json:"password"`
	Port         int      `json:"port"`
	UseTLS       bool     `json:"use_tls"`
	Analytics    []string `json:"analytics,omitempty"`
	Enabled      bool     `json:"enabled"`

	RTSPURL                string `json:"rtsp_url,omitempty"`
	ProxyPath              string `json:"proxy_path,omitempty"`
	CentralPath            string `json:"central_path,omitempty"`
	RecordEnabled          bool   `json:"record_enabled,omitempty"`
	RecordRetentionMinutes int    `json:"record_retention_minutes,omitempty"`
	PreRollSeconds         int    `json:"pre_roll_seconds,omitempty"`

	// Enriquecido pelo supervisor a partir do tópico /info
	Tenant     string `json:"tenant"`
	Building   string `json:"building"`
	Floor      string `json:"floor"`
	DeviceType string `json:"device_type"`
	DeviceID   string `json:"device_id"`

	// Shard responsável por essa câmera (ex.: "shard-1", "shard-2", "ceara-sede", etc.)
	Shard string `json:"shard,omitempty"`
}

type AnalyticEvent struct {
	Timestamp    time.Time `json:"Timestamp"`
	EventID      string    `json:"EventID"`
	CameraIP     string    `json:"CameraIP"`
	CameraName   string    `json:"CameraName"`
	AnalyticType string    `json:"AnalyticType"`

	// Contexto da câmera (copiado do CameraInfo)
	Tenant     string `json:"Tenant,omitempty"`
	Building   string `json:"Building,omitempty"`
	Floor      string `json:"Floor,omitempty"`
	DeviceType string `json:"DeviceType,omitempty"`
	DeviceID   string `json:"DeviceID,omitempty"`

	// Metadados genéricos por evento (score, channel, etc.)
	Meta map[string]interface{} `json:"Meta"`

	// URL pública do snapshot no MinIO
	SnapshotURL string `json:"SnapshotURL,omitempty"`

	// Legacy / debug only – base64 do snapshot, se quiser manter
	SnapshotB64 string `json:"SnapshotB64,omitempty"`

	// ⚠️ Novo: bytes crus do snapshot em memória (NÃO vai pro JSON / MQTT)
	RawSnapshot []byte `json:"-"` // usado internamente pelo face engine (FindFace)
}
