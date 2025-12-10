// cmd/face-router/main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/sua-org/cam-bus/internal/core"
	"github.com/sua-org/cam-bus/internal/mqttclient"
	"github.com/sua-org/cam-bus/internal/recognition"
)

func main() {
    if err := godotenv.Load(); err != nil {
        log.Printf("[face-router] aviso: não foi possível carregar .env: %v", err)
    } else {
        log.Printf("[face-router] .env carregado com sucesso")
    }

    baseTopic := getenv("MQTT_BASE_TOPIC", "rtls/cameras")

    mqttCli, err := mqttclient.NewClientFromEnv("face-router")
    if err != nil {
        log.Fatalf("erro ao conectar no MQTT: %v", err)
    }
    defer mqttCli.Close()

    engineName := getenv("FACE_ENGINE", "findface")
    eng, err := buildEngine(engineName)
    if err != nil {
        log.Fatalf("erro ao inicializar engine %s: %v", engineName, err)
    }
    log.Printf("[face-router] usando engine: %s", eng.Name())

    subTopic := fmt.Sprintf("$share/face-router/%s/+/+/+/+/+/faceCapture/events", baseTopic)
    log.Printf("[face-router] subscrevendo em: %s", subTopic)

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    sig := make(chan os.Signal, 1)
    signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

    if err := mqttCli.Subscribe(subTopic, 1, func(topic string, payload []byte) {
        handleMessage(ctx, mqttCli, baseTopic, eng, topic, payload)
    }); err != nil {
        log.Fatalf("erro ao assinar tópico %s: %v", subTopic, err)
    }

    go func() {
        <-sig
        log.Println("[face-router] sinal recebido, encerrando...")
        cancel()
    }()

    <-ctx.Done()
    time.Sleep(500 * time.Millisecond)
}

func buildEngine(name string) (recognition.Engine, error) {
    switch name {
    case "findface":
        return recognition.NewFindFaceFromEnv()
    // case "dahua_ivss":
    //     return recognition.NewDahuaIVSSEngineFromEnv()
    // case "deepneuronic":
    //     return recognition.NewDeepNeuronicFromEnv()
    default:
        return recognition.NewFindFaceFromEnv()
    }
}

func handleMessage(
    ctx context.Context,
    mqttCli *mqttclient.Client,
    baseTopic string,
    engine recognition.Engine,
    topic string,
    payload []byte,
) {
    var evt core.AnalyticEvent
    if err := json.Unmarshal(payload, &evt); err != nil {
        log.Printf("[face-router] erro ao decodificar JSON: %v", err)
        return
    }

    if evt.AnalyticType != "faceCapture" {
        return
    }

    ctxReq, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()

    res, err := engine.HandleFaceCapture(ctxReq, evt)
    if err != nil {
        log.Printf("[face-router] erro no engine %s: %v", engine.Name(), err)
        return
    }
    if res == nil {
        return
    }

    out := recognition.FaceRecognitionEvent{
        Timestamp:     time.Now().UTC(),
        SourceEventID: evt.EventID,
        CameraIP:      evt.CameraIP,
        CameraName:    evt.CameraName,
        Tenant:        evt.Tenant,
        Building:      evt.Building,
        Floor:         evt.Floor,
        DeviceType:    evt.DeviceType,
        DeviceID:      evt.DeviceID,
        SnapshotURL:   evt.SnapshotURL,
        Recognition:   res,
    }

    b, err := json.Marshal(out)
    if err != nil {
        log.Printf("[face-router] erro ao montar JSON: %v", err)
        return
    }

    topicOut := fmt.Sprintf("%s/%s/%s/%s/%s/%s/FaceRecognized/events",
        baseTopic,
        safe(evt.Tenant, "default"),
        safe(evt.Building, "building"),
        safe(evt.Floor, "floor"),
        safe(evt.DeviceType, "device"),
        safe(evt.DeviceID, "id"),
    )

    if err := mqttCli.Publish(topicOut, 1, false, b); err != nil {
        log.Printf("[face-router] erro ao publicar em %s: %v", topicOut, err)
    } else {
        log.Printf("[face-router] published FaceRecognized -> %s (event=%s, matched=%v, person=%s)",
            topicOut, evt.EventID, res.Matched, res.PersonName)
    }
}

func getenv(k, def string) string {
    if v := os.Getenv(k); v != "" {
        return v
    }
    return def
}

func safe(v, def string) string {
    if v == "" {
        return def
    }
    return v
}
