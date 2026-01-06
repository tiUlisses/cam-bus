// cmd/cam-bus/main.go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/sua-org/cam-bus/internal/mqttclient"
	"github.com/sua-org/cam-bus/internal/storage"
	"github.com/sua-org/cam-bus/internal/supervisor"
	"github.com/sua-org/cam-bus/internal/uplink"
)

func main() {
	// Carrega .env na raiz (se não existir, só loga aviso)
	if err := godotenv.Load(); err != nil {
		log.Printf("[main] aviso: não foi possível carregar .env: %v", err)
	} else {
		log.Printf("[main] .env carregado com sucesso")
	}

	baseTopic := getenv("MQTT_BASE_TOPIC", "security-vision/cameras")

	// Inicializa MinIO (opcional; se falhar, continua sem storage remoto)
	store, err := storage.NewMinioStoreFromEnv()
	if err != nil {
		log.Printf("[main] aviso: MinIO não inicializado: %v", err)
	} else {
		storage.DefaultStore = store
	}

	mqttCli, err := mqttclient.NewClientFromEnv("cam-bus")
	if err != nil {
		log.Fatalf("erro ao conectar no MQTT: %v", err)
	}
	defer mqttCli.Close()

	sup := supervisor.New(mqttCli, baseTopic)
	uplinkMgr := uplink.NewManager(mqttCli, baseTopic)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := sup.Run(ctx); err != nil {
			log.Printf("[main] supervisor terminou com erro: %v", err)
		}
	}()
	go func() {
		if err := uplinkMgr.Run(ctx); err != nil {
			log.Printf("[main] uplink manager terminou com erro: %v", err)
		}
	}()

	<-sig
	log.Println("[main] sinal recebido, encerrando...")
	cancel()
	time.Sleep(1 * time.Second)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
