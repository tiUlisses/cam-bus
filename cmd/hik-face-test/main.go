package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sua-org/cam-bus/internal/core"
	"github.com/sua-org/cam-bus/internal/drivers"
)

func main() {
    // Ajuste aqui o usuário/senha da câmera
    info := core.CameraInfo{
        IP:           "192.168.94.204",
        Name:         "PTZ_FACE_01",
        Manufacturer: "Hikvision",
        Model:        "PTZ",
        Username:     "admin",      // AJUSTAR
        Password:     "h0wb3@12",  // AJUSTAR
        Port:         80,
        UseTLS:       false,
        Analytics:    []string{"faceCapture"},
        Enabled:      true,
    }

    drv, err := drivers.GetDriver(info)
    if err != nil {
        log.Fatalf("erro ao obter driver: %v", err)
    }

    events := make(chan core.AnalyticEvent, 10)

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // Consumidor: loga eventos e salva snapshot, se existir
    go func() {
        for evt := range events {
            log.Printf(
                "[EVENT] %s IP=%s analytic=%s faces=%v score=%v snapshot?=%v",
                evt.Timestamp.Format(time.RFC3339),
                evt.CameraIP,
                evt.AnalyticType,
                evt.Meta["facesCount"],
                evt.Meta["bestScore"],
                evt.SnapshotB64 != "",
            )

            if evt.SnapshotB64 != "" {
                data, err := base64.StdEncoding.DecodeString(evt.SnapshotB64)
                if err != nil {
                    log.Printf("erro ao decodificar snapshot base64: %v", err)
                    continue
                }

                filename := fmt.Sprintf("snapshot_%s.jpg", evt.EventID)
                if err := os.WriteFile(filename, data, 0o644); err != nil {
                    log.Printf("erro ao salvar snapshot em disco: %v", err)
                    continue
                }

                log.Printf("snapshot salvo em: %s", filename)
            }
        }
    }()

    // Ctrl+C pra sair
    sig := make(chan os.Signal, 1)
    signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

    go func() {
        if err := drv.Run(ctx, events); err != nil {
            log.Printf("driver encerrou com erro: %v", err)
        }
        close(events)
    }()

    <-sig
    log.Println("encerrando teste...")
    cancel()
    time.Sleep(1 * time.Second)
}
