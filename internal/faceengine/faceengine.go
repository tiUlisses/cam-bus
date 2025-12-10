// internal/faceengine/faceengine.go
package faceengine

import (
	"context"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sua-org/cam-bus/internal/core"
	ff "github.com/sua-org/cam-bus/internal/findface"
)

// Engine é a fachada de alto nível para o FindFace.
type Engine struct {
	client *ff.Client
}

// NewFromEnv inicializa o engine de face usando o client do FindFace.
// Usa a variável FACE_ENGINE para decidir se liga/desliga.
func NewFromEnv() *Engine {
	engineName := strings.ToLower(strings.TrimSpace(os.Getenv("FACE_ENGINE")))
	if engineName == "" || engineName == "none" {
		log.Printf("[faceengine] FACE_ENGINE vazio ou 'none', engine desabilitado")
		return nil
	}
	if engineName != "findface" {
		log.Printf("[faceengine] FACE_ENGINE=%s não suportado (por enquanto só 'findface')", engineName)
		return nil
	}

	client, err := ff.NewFromEnv()
	if err != nil {
		log.Printf("[faceengine] erro criando client FindFace: %v", err)
		return nil
	}

	log.Printf("[faceengine] iniciado com FindFace em %s (camera_id=%d)",
		client.BaseURL, client.CameraID)

	return &Engine{client: client}
}

// Enabled retorna true se o engine está ativo.
func (e *Engine) Enabled() bool {
	return e != nil && e.client != nil
}

// ProcessFaceCapture:
// - recebe um AnalyticEvent (faceCapture da Hikvision OU FaceDetection da Dahua);
// - carrega o snapshot (SnapshotB64 ou SnapshotURL);
// - envia para o FindFace via CreateFaceEventFromBytes;
// - consulta detalhes do evento + card;
// - se houver match, devolve um novo AnalyticEvent com AnalyticType = "faceRecognized";
// - se não houver match ou der "zero faces", retorna (nil, nil).
func (e *Engine) ProcessFaceCapture(
	ctx context.Context,
	evt core.AnalyticEvent,
) (*core.AnalyticEvent, error) {
	if e == nil || e.client == nil {
		return nil, nil
	}

	at := strings.ToLower(strings.TrimSpace(evt.AnalyticType))
	if at != "facecapture" && at != "facedetection" {
		// não é evento de face => ignoramos
		return nil, nil
	}

	// 1) tenta primeiro via SnapshotB64 (Hikvision e Dahua agora preenchem isso)
	var img []byte
	if evt.SnapshotB64 != "" {
		data, err := base64.StdEncoding.DecodeString(evt.SnapshotB64)
		if err != nil {
			log.Printf("[faceengine] erro ao decodificar SnapshotB64: %v", err)
		} else {
			img = data
		}
	}

	// 2) fallback: tenta baixar SnapshotURL se não tiver base64
	if len(img) == 0 && evt.SnapshotURL != "" {
		httpCli := &http.Client{Timeout: 5 * time.Second}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, evt.SnapshotURL, nil)
		if err == nil {
			resp, err := httpCli.Do(req)
			if err != nil {
				log.Printf("[faceengine] erro HTTP ao baixar SnapshotURL: %v", err)
			} else {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					img, err = io.ReadAll(resp.Body)
					if err != nil {
						log.Printf("[faceengine] erro ao ler SnapshotURL: %v", err)
					}
				} else {
					body, _ := io.ReadAll(resp.Body)
					log.Printf("[faceengine] SnapshotURL status %d: %s", resp.StatusCode, string(body))
				}
			}
		}
	}

	if len(img) == 0 {
		log.Printf("[faceengine] %s sem snapshot, nada para enviar ao FindFace", evt.AnalyticType)
		return nil, nil
	}

	// 3) Cria evento de face no FindFace
	res, err := e.client.CreateFaceEventFromBytes(ctx, img, "snapshot.jpg")
	if err != nil {
		// Se for "Zero objects(type=\"face\") detected...", tratamos como “sem rosto”
		if strings.Contains(err.Error(), `Zero objects(type="face")`) ||
			strings.Contains(err.Error(), `Zero objects(type=\"face\")`) {
			log.Printf("[faceengine] FindFace retornou zero faces para o snapshot (event_id? unknown, evt_id=%s)", evt.EventID)
			return nil, nil
		}

		log.Printf("[faceengine] erro ao criar evento de face no FindFace: %v", err)
		return nil, err
	}
	if res == nil || strings.TrimSpace(res.EventID) == "" {
		// sem ID de evento, não dá pra consultar match
		log.Printf("[faceengine] CreateFaceEventFromBytes retornou sem EventID (evt_id=%s)", evt.EventID)
		return nil, nil
	}

	// 4) Consulta detalhes do evento de face
	fevent, err := e.client.GetFaceEvent(ctx, res.EventID)
	if err != nil {
		log.Printf("[faceengine] erro ao consultar GetFaceEvent(%s): %v", res.EventID, err)
		// não tratamos como erro fatal de pipeline, só logamos
		return nil, nil
	}

	if !fevent.Matched || fevent.MatchedCard == nil {
		// evento sem match em nenhum card
		return nil, nil
	}

    // 5) Consulta card (pessoa) correspondente
    cardID := *fevent.MatchedCard
    card, err := e.client.GetCard(ctx, cardID)
    if err != nil {
        log.Printf("[faceengine] erro ao consultar GetCard(%d): %v", cardID, err)
    }

    // Nome da pessoa
    personName := ""
    if card != nil {
        personName = e.client.GetCardName(card)
    }

    // 5.1) Tenta buscar um objeto de face ligado a esse card (foto cadastrada na base)
    var personPhotoURL string
    faceObj, err := e.client.GetFaceObjectForCard(ctx, cardID)
    if err != nil {
        log.Printf("[faceengine] erro ao consultar GetFaceObjectForCard(%d): %v", cardID, err)
    } else if faceObj != nil {
        // Prioriza source_photo (foto inteira); se não tiver, cai no thumbnail
        if strings.TrimSpace(faceObj.SourcePhoto) != "" {
            personPhotoURL = strings.TrimSpace(faceObj.SourcePhoto)
        } else if strings.TrimSpace(faceObj.Thumbnail) != "" {
            personPhotoURL = strings.TrimSpace(faceObj.Thumbnail)
        }
    }

    // 5.2) Fallback: tenta extrair URL de foto diretamente do card (features/meta)
    if personPhotoURL == "" && card != nil {
        if url := e.client.GetCardPhotoURL(card); url != "" {
            personPhotoURL = url
        }
    }

    // Confiança
    conf := fevent.Confidence
    if fevent.LooksLikeConf != nil {
        conf = *fevent.LooksLikeConf
    }

    // 6) Monta evento "faceRecognized" reaproveitando o contexto do evento original.
    recognized := evt
    recognized.AnalyticType = "faceRecognized"
    if recognized.Meta == nil {
        recognized.Meta = map[string]interface{}{}
    }

    recognized.Meta["ff_event_id"] = fevent.ID
    recognized.Meta["ff_matched"] = fevent.Matched
    recognized.Meta["ff_card_id"] = cardID
    recognized.Meta["ff_person_name"] = personName
    recognized.Meta["ff_confidence"] = conf

    // FOTO DO CADASTRO (base FindFace)
    if personPhotoURL != "" {
        recognized.Meta["ff_person_photo_url"] = personPhotoURL
    }

    log.Printf("[faceengine] faceRecognized: event=%s card=%v name=%q conf=%.4f photo=%q",
        fevent.ID, cardID, personName, conf, personPhotoURL)

    return &recognized, nil
}
