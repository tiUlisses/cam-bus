package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sua-org/cam-bus/internal/mqttclient"
)

func main() {
    baseTopic := getenv("MQTT_BASE_TOPIC", "rtls/cameras")

    // Só eventos:
    // base/tenant/building/floor/type/id/analytic/events
    defaultDebugTopic := baseTopic + "/+/+/+/+/+/+/events"
    subscribeTopic := getenv("MQTT_DEBUG_TOPIC", defaultDebugTopic)

    mqttCli, err := mqttclient.NewClientFromEnv("cam-bus-debug-subscriber")
    if err != nil {
        log.Fatalf("erro ao conectar no MQTT: %v", err)
    }
    defer mqttCli.Close()

    log.Printf("[debug] subscribed to topic: %s", subscribeTopic)

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    sig := make(chan os.Signal, 1)
    signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

    if err := mqttCli.Subscribe(subscribeTopic, 1,
        func(topic string, payload []byte) {
            handleMessage(topic, payload)
        },
    ); err != nil {
        log.Fatalf("erro ao assinar tópico %s: %v", subscribeTopic, err)
    }

    go func() {
        <-sig
        log.Println("[debug] sinal recebido, encerrando subscriber...")
        cancel()
    }()

    <-ctx.Done()
    time.Sleep(500 * time.Millisecond)
}

func handleMessage(topic string, payload []byte) {
    log.Printf("\n[debug] mensagem recebida no tópico: %s", topic)
    log.Printf("[debug] payload bruto (%d bytes)", len(payload))

    // Decodifica JSON genérico
    var raw map[string]interface{}
    if err := json.Unmarshal(payload, &raw); err != nil {
        log.Printf("[debug] erro ao fazer unmarshal do JSON: %v", err)
        log.Printf("[debug] payload como string: %s", string(payload))
        return
    }

    // Mostra JSON bonitinho
    pretty, _ := json.MarshalIndent(raw, "", "  ")
    log.Printf("[debug] JSON decodificado:\n%s", string(pretty))

    // Pega alguns campos “comuns” se existirem
    ts := getString(raw, "Timestamp", "ts", "time", "timestamp")
    camIP := getString(raw, "CameraIP", "camera_ip", "ip")
    camName := getString(raw, "CameraName", "camera_name", "name")
    analytic := getString(raw, "AnalyticType", "analytic_type", "type", "eventType")

    log.Printf("[EVENT] ts=%s ip=%s name=%s analytic=%s", ts, camIP, camName, analytic)

    // Snapshot pode vir com vários nomes, tentamos todos
    snap := getString(raw, "SnapshotB64", "snapshot_b64", "snapshot", "image_b64")
    if snap == "" {
        log.Printf("[EVENT] sem snapshot em base64 (nenhuma chave conhecida encontrada).")
        return
    }

    data, err := base64.StdEncoding.DecodeString(snap)
    if err != nil {
        log.Printf("[EVENT] erro ao decodificar snapshot base64: %v", err)
        return
    }

    if analytic == "" {
        analytic = "analytic"
    }

    // Gera nome simples com timestamp
    filename := fmt.Sprintf(
        "mqtt_snapshot_%s_%d.jpg",
        analytic,
        time.Now().UnixNano(),
    )

    if err := os.WriteFile(filename, data, 0o644); err != nil {
        log.Printf("[EVENT] erro ao salvar snapshot em disco: %v", err)
        return
    }

    log.Printf("[EVENT] snapshot salvo em: %s\n", filename)
}

func getString(m map[string]interface{}, keys ...string) string {
    for _, k := range keys {
        if v, ok := m[k]; ok {
            if s, ok := v.(string); ok {
                return s
            }
        }
    }
    return ""
}

func getenv(key, def string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return def
}
