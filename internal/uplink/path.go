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
