package engines

import (
    "context"

    "github.com/sua-org/cam-bus/internal/core"
    "github.com/sua-org/cam-bus/internal/faceengine"
)

// FindFaceEngine adapta o pacote internal/faceengine para o padrão Engine.
// Isso facilita futuramente trocar/alternar engines (ex.: DeepNeuronic, IVSS, etc.)
// sem tocar no supervisor.
type FindFaceEngine struct {
    fe *faceengine.Engine
}

func NewFindFaceFromEnv() Engine {
    fe := faceengine.NewFromEnv()
    if fe == nil || !fe.Enabled() {
        return nil
    }
    return &FindFaceEngine{fe: fe}
}

func (e *FindFaceEngine) Name() string { return "findface" }

func (e *FindFaceEngine) Enabled() bool { return e != nil && e.fe != nil && e.fe.Enabled() }

func (e *FindFaceEngine) Process(ctx context.Context, evt core.AnalyticEvent) ([]core.AnalyticEvent, error) {
    if !e.Enabled() {
        return nil, nil
    }
    out, err := e.fe.ProcessFaceCapture(ctx, evt)
    if err != nil || out == nil {
        return nil, err
    }
    return []core.AnalyticEvent{*out}, nil
}
