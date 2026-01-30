package uplink

import (
	"path"
	"strings"

	"github.com/sua-org/cam-bus/internal/core"
)

// CentralPathFor monta o caminho central no padrão tenant/building/deviceId.
func CentralPathFor(info core.CameraInfo) string {
	tenant := strings.Trim(strings.TrimSpace(info.Tenant), "/")
	building := strings.Trim(strings.TrimSpace(info.Building), "/")
	deviceID := strings.Trim(strings.TrimSpace(info.DeviceID), "/")
	return path.Join(tenant, building, deviceID)
}

// DefaultCentralPath junta um path base com um sufixo por câmera (proxyPath ou deviceId).
func DefaultCentralPath(basePath string, info core.CameraInfo) string {
	base := strings.Trim(strings.TrimSpace(basePath), "/")
	if base == "" {
		return ""
	}
	suffix := strings.Trim(strings.TrimSpace(info.ProxyPath), "/")
	if suffix == "" {
		suffix = strings.Trim(strings.TrimSpace(info.DeviceID), "/")
	}
	if suffix == "" {
		return ""
	}
	return path.Join(base, suffix)
}
