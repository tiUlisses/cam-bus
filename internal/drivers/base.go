// internal/drivers/base.go
package drivers

import (
	"context"

	"github.com/sua-org/cam-bus/internal/core"
)

type CameraDriver interface {
	// Run deve rodar o loop de eventos da câmera até o ctx ser cancelado ou ocorrer erro fatal
	Run(ctx context.Context, events chan<- core.AnalyticEvent) error
}

// ConnectionState representa o estado atual de conectividade com a câmera.
// Os valores são publicados pelo supervisor para consumo externo.
type ConnectionState string

const (
	ConnectionStateConnecting     ConnectionState = "connecting"
	ConnectionStateOnline         ConnectionState = "online"
	ConnectionStateOffline        ConnectionState = "offline"
	ConnectionStateNotEstablished ConnectionState = "not_established"
)

// StatusUpdate é usado pelos drivers para reportar mudanças de conectividade.
type StatusUpdate struct {
	State  ConnectionState
	Reason string
}

// StatusAwareDriver permite que o supervisor receba atualizações do driver em tempo real.
type StatusAwareDriver interface {
	SetStatusHandler(func(StatusUpdate))
}

// AnalyticsReporter expõe quais analytics o driver realmente assinou.
type AnalyticsReporter interface {
	ActiveAnalytics() []string
}

type DriverFactory func(info core.CameraInfo) (CameraDriver, error)

// registry: fabricante:model -> factory
var registry = map[string]DriverFactory{}

// RegisterDriver é chamado no init() de cada driver (Hikvision, Dahua, etc).
func RegisterDriver(manufacturer, model string, f DriverFactory) {
	registry[normalize(manufacturer)+":"+normalize(model)] = f
}

func GetDriver(info core.CameraInfo) (CameraDriver, error) {
	if f, ok := registry[keyFor(info)]; ok {
		return f(info)
	}
	// fallback: fabricante:any
	if f, ok := registry[normalize(info.Manufacturer)+":any"]; ok {
		return f(info)
	}
	return nil, ErrDriverNotFound
}

func keyFor(info core.CameraInfo) string {
	return normalize(info.Manufacturer) + ":" + normalize(info.Model)
}

func normalize(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range []rune(s) {
		// remove espaços, hífen, underline
		if r == ' ' || r == '-' || r == '_' {
			continue
		}
		// minúsculo
		if r >= 'A' && r <= 'Z' {
			r = r + 32
		}
		b = append(b, r)
	}
	return string(b)
}
