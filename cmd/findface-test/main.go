// cmd/findface-test/main.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	"github.com/sua-org/cam-bus/internal/findface"
)

func main() {
	// Carrega .env se existir
	if err := godotenv.Load(); err == nil {
		log.Printf("[findface-test] .env carregado com sucesso")
	}

	if len(os.Args) < 2 {
		log.Fatalf("uso: go run ./cmd/findface-test <caminho_da_imagem>")
	}
	imagePath := os.Args[1]

	client, err := findface.NewFromEnv()
	if err != nil {
		log.Fatalf("erro ao criar client FindFace: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1) Cria o evento de face
	resp, err := client.CreateFaceEventFromFile(ctx, imagePath)
	if err != nil {
		log.Fatalf("erro ao criar face event: %v", err)
	}

	log.Printf("faces/add OK. EventID detectado: %q", resp.EventID)

	if len(resp.RawJSON) > 0 {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, resp.RawJSON, "", "  "); err != nil {
			log.Printf("faces/add raw body: %s", string(resp.RawJSON))
		} else {
			log.Printf("faces/add JSON de resposta:\n%s", pretty.String())
		}
	} else {
		log.Printf("faces/add não retornou corpo (body vazio).")
	}

	// 2) Se tiver ID, busca os detalhes do evento (match, card, confiança, etc.)
	if resp.EventID == "" {
		log.Printf("Nenhum EventID detectado, não dá pra buscar match agora.")
		return
	}

	// Mini delay opcional
	time.Sleep(500 * time.Millisecond)

	evt, err := client.GetFaceEvent(ctx, resp.EventID)
	if err != nil {
		log.Fatalf("erro ao buscar detalhes do evento %s: %v", resp.EventID, err)
	}

	// Mostra o resumo
	matchedCardStr := "nil"
	if evt.MatchedCard != nil {
		matchedCardStr = strconv.Itoa(*evt.MatchedCard)
	}

	log.Printf("Detalhes do evento %s:", evt.ID)
	log.Printf("- matched        : %v", evt.Matched)
	log.Printf("- matched_card   : %s", matchedCardStr)
	log.Printf("- confidence     : %.4f", evt.Confidence)
	if evt.LooksLikeConf != nil {
		log.Printf("- looks_like_conf: %.4f", *evt.LooksLikeConf)
	}
	log.Printf("- thumbnail      : %s", evt.Thumbnail)
	log.Printf("- fullframe      : %s", evt.Fullframe)

	// 3) Se houve match, busca o card e o nome da pessoa
	if evt.Matched && evt.MatchedCard != nil {
		card, err := client.GetCard(ctx, *evt.MatchedCard)
		if err != nil {
			log.Printf("erro ao buscar card %d: %v", *evt.MatchedCard, err)
		} else {
			name := client.GetCardName(card)
			log.Printf("- card_name      : %s", name)

			// Imprime JSON bruto do card (opcional, bom pra ver como vem "features")
			cardJSON, _ := json.MarshalIndent(card, "", "  ")
			log.Printf("Card completo:\n%s", string(cardJSON))
		}
	}

	// Opcional: imprimir JSON completo do evento (struct)
	evtJSON, _ := json.MarshalIndent(evt, "", "  ")
	log.Printf("Evento completo (struct):\n%s", string(evtJSON))
}
