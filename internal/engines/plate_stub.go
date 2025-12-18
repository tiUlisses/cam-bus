package engines

import (
    "context"

    "github.com/sua-org/cam-bus/internal/core"
)

// PlateStub é um placeholder para a engine de placas.
//
// Ele existe para já deixar o projeto modular e permitir alternância via ENGINES,
// sem bloquear o desenvolvimento. No patch que for integrar de verdade, a gente
// troca essa implementação por uma engine real.
type PlateStub struct{}

func NewPlateStub() Engine { return &PlateStub{} }

func (p *PlateStub) Name() string { return "plater" }

func (p *PlateStub) Enabled() bool { return true }

func (p *PlateStub) Process(ctx context.Context, evt core.AnalyticEvent) ([]core.AnalyticEvent, error) {
    // ainda não faz nada.
    return nil, nil
}
