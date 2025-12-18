package engines

import (
    "context"

    "github.com/sua-org/cam-bus/internal/core"
)

// Engine é um pós-processador de eventos.
// Ele recebe um evento (ex.: faceCapture, ANPR, etc.) e pode:
// - retornar zero eventos (ignorar)
// - retornar um ou mais eventos derivados (ex.: faceRecognized)
//
// Importante: engines NÃO publicam no MQTT diretamente.
// Quem publica é o supervisor, para manter consistência de tópicos.
type Engine interface {
    Name() string
    Enabled() bool

    // Process pode retornar:
    // - nil, nil  => engine não aplicável / sem saída
    // - []events  => eventos derivados
    // - error     => erro (o supervisor decide se loga e segue)
    Process(ctx context.Context, evt core.AnalyticEvent) ([]core.AnalyticEvent, error)
}
