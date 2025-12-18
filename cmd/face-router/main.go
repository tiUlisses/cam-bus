// cmd/face-router/main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/sua-org/cam-bus/internal/core"
	"github.com/sua-org/cam-bus/internal/engines"
	"github.com/sua-org/cam-bus/internal/mqttclient"
)

func main() {
    if err := godotenv.Load(); err != nil {
        log.Printf("[face-router] aviso: não foi possível carregar .env: %v", err)
    } else {
        log.Printf("[face-router] .env carregado com sucesso")
    }

    baseTopic := getenv("MQTT_BASE_TOPIC", "security-vision/cameras")

    mqttCli, err := mqttclient.NewClientFromEnv("face-router")
    if err != nil {
        log.Fatalf("erro ao conectar no MQTT: %v", err)
    }
    defer mqttCli.Close()

    mgr := engines.LoadFromEnv()
    if mgr == nil || !mgr.Enabled() {
        log.Fatalf("[face-router] nenhuma engine habilitada (use ENGINES=findface ou FACE_ENGINE=findface)")
    }
    log.Printf("[face-router] engines habilitadas: %v", mgr.Names())

    subTopic := fmt.Sprintf("$share/face-router/%s/+/+/+/+/+/faceCapture/events", baseTopic)
    log.Printf("[face-router] subscrevendo em: %s", subTopic)

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    sig := make(chan os.Signal, 1)
    signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

    if err := mqttCli.Subscribe(subTopic, 1, func(topic string, payload []byte) {
        handleMessage(ctx, mqttCli, baseTopic, mgr, topic, payload)
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

func handleMessage(
    ctx context.Context,
    mqttCli *mqttclient.Client,
    baseTopic string,
    mgr *engines.Manager,
    topic string,
    payload []byte,
) {
    var evt core.AnalyticEvent
    if err := json.Unmarshal(payload, &evt); err != nil {
        log.Printf("[face-router] erro ao decodificar JSON: %v", err)
        return
    }

    at := strings.ToLower(strings.TrimSpace(evt.AnalyticType))
    if at != "facecapture" && at != "facedetection" {
        return
    }

    ctxReq, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()

    derived, _ := mgr.ProcessAll(ctxReq, evt)
    for _, d := range derived {
        // Publica sem SnapshotB64 (evitar explosão no MQTT)
        out := d
        out.SnapshotB64 = ""

        b, err := json.Marshal(out)
        if err != nil {
            log.Printf("[face-router] erro ao montar JSON (%s): %v", out.AnalyticType, err)
            continue
        }

        topicOut := fmt.Sprintf("%s/%s/%s/%s/%s/%s/%s/events",
            baseTopic,
            safe(evt.Tenant, "default"),
            safe(evt.Building, "building"),
            safe(evt.Floor, "floor"),
            safe(evt.DeviceType, "device"),
            safe(evt.DeviceID, "id"),
            safe(out.AnalyticType, "unknown"),
        )

        if err := mqttCli.Publish(topicOut, 1, false, b); err != nil {
            log.Printf("[face-router] erro ao publicar em %s: %v", topicOut, err)
        } else {
            log.Printf("[face-router] published %s -> %s (source_event=%s)", out.AnalyticType, topicOut, evt.EventID)
        }
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
