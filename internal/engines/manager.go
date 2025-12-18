package engines

import (
    "context"
    "fmt"
    "log"
    "runtime/debug"
    "strings"
    "time"

    "github.com/sua-org/cam-bus/internal/core"
)

type Manager struct {
    engines []Engine

    // timeout padrão para cada engine
    perEngineTimeout time.Duration
}

func NewManager(engines []Engine, perEngineTimeout time.Duration) *Manager {
    if perEngineTimeout <= 0 {
        perEngineTimeout = 10 * time.Second
    }
    // remove nils e engines desabilitados
    filtered := make([]Engine, 0, len(engines))
    for _, e := range engines {
        if e == nil || !e.Enabled() {
            continue
        }
        filtered = append(filtered, e)
    }
    return &Manager{engines: filtered, perEngineTimeout: perEngineTimeout}
}

func (m *Manager) Enabled() bool {
    return m != nil && len(m.engines) > 0
}

func (m *Manager) Names() []string {
    if m == nil {
        return nil
    }
    out := make([]string, 0, len(m.engines))
    for _, e := range m.engines {
        out = append(out, e.Name())
    }
    return out
}

func (m *Manager) Has(name string) bool {
    if m == nil {
        return false
    }
    name = strings.ToLower(strings.TrimSpace(name))
    for _, e := range m.engines {
        if strings.ToLower(e.Name()) == name {
            return true
        }
    }
    return false
}

// ProcessAll roda todas as engines em sequência e retorna todos os eventos derivados.
// Nunca dá panic (proteção de recover por engine).
func (m *Manager) ProcessAll(ctx context.Context, evt core.AnalyticEvent) ([]core.AnalyticEvent, error) {
    if m == nil || len(m.engines) == 0 {
        return nil, nil
    }

    var out []core.AnalyticEvent
    for _, e := range m.engines {
        if e == nil || !e.Enabled() {
            continue
        }

        // Timeout por engine para não travar o pipeline
        ctxEng, cancel := context.WithTimeout(ctx, m.perEngineTimeout)
        derived, err := func() (res []core.AnalyticEvent, err error) {
            defer func() {
                if r := recover(); r != nil {
                    log.Printf("[engines] panic na engine %s: %v\n%s", e.Name(), r, string(debug.Stack()))
                    err = fmt.Errorf("panic in engine %s", e.Name())
                }
            }()
            return e.Process(ctxEng, evt)
        }()
        cancel()

        if err != nil {
            // por enquanto: loga e segue (não falha o worker)
            log.Printf("[engines] engine %s erro: %v", e.Name(), err)
            continue
        }
        if len(derived) > 0 {
            out = append(out, derived...)
        }
    }
    return out, nil
}
