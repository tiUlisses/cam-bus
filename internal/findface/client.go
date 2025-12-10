// internal/findface/client.go
package findface

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client representa um cliente simples para o FindFace Multi.
type Client struct {
	BaseURL     string
	APIToken    string // token de API (Authorization: Token ...)
	EventsToken string // token de criação de eventos (campo "token")
	CameraID    int
	NameField   string // chave em features que contém o "nome" (ex: "name")

	HTTP *http.Client
}

// CreateFaceEventResponse guarda o que recebemos do /events/faces/add.
type CreateFaceEventResponse struct {
	EventID string          // se conseguirmos extrair algum ID, vem aqui
	RawJSON json.RawMessage // corpo bruto da resposta (para debug / uso futuro)
}

// FaceEvent representa (parcialmente) um evento de face retornado por /events/faces/.
type FaceEvent struct {
	ID            string   `json:"id"`
	Matched       bool     `json:"matched"`
	MatchedCard   *int     `json:"matched_card"`
	MatchedLists  []int    `json:"matched_lists"`
	Confidence    float64  `json:"confidence"`
	LooksLikeConf *float64 `json:"looks_like_confidence"`
	Thumbnail     string   `json:"thumbnail"`
	Fullframe     string   `json:"fullframe"`
}

// Card representa (parcialmente) um card (pessoa) no FindFace.
type Card struct {
	ID       int                    `json:"id"`
	Name     *string                `json:"name,omitempty"` // se existir direto no root
	Features map[string]interface{} `json:"features"`
	Meta     map[string]interface{} `json:"meta"`
}

// FaceObject representa (parcialmente) um objeto de face em /objects/faces/.
type FaceObject struct {
    ID          string                 `json:"id"`
    Card        int                    `json:"card"`
    Thumbnail   string                 `json:"thumbnail"`
    SourcePhoto string                 `json:"source_photo"`
    Features    map[string]interface{} `json:"features"`
    Meta        map[string]interface{} `json:"meta"`
}

// New cria um client com parâmetros explícitos.
func New(baseURL, apiToken, eventsToken string, cameraID int, nameField string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	if nameField == "" {
		nameField = "name"
	}
	return &Client{
		BaseURL:     baseURL,
		APIToken:    apiToken,
		EventsToken: eventsToken,
		CameraID:    cameraID,
		NameField:   nameField,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewFromEnv cria um client lendo variáveis de ambiente:
//
//   FINDFACE_BASE_URL         (ex: http://10.10.0.35)
//   FINDFACE_API_TOKEN        (opcional; se vazio, cai em FINDFACE_EXTERNAL_TOKEN)
//   FINDFACE_EXTERNAL_TOKEN   (token de API se você não usar FINDFACE_API_TOKEN)
//   FINDFACE_EVENTS_TOKEN     (token de criação de eventos do external detector)
//   FINDFACE_CAMERA_ID        (id da câmera no FindFace, ex: 47)
//   FINDFACE_NAME_FIELD       (chave dentro de features com o nome da pessoa, default: "name")
func NewFromEnv() (*Client, error) {
	baseURL := os.Getenv("FINDFACE_BASE_URL")
	if baseURL == "" {
		return nil, fmt.Errorf("FINDFACE_BASE_URL não definido")
	}

	apiToken := os.Getenv("FINDFACE_API_TOKEN")
	if apiToken == "" {
		// fallback pra manter compat com teu .env atual
		apiToken = os.Getenv("FINDFACE_EXTERNAL_TOKEN")
	}
	if apiToken == "" {
		return nil, fmt.Errorf("defina FINDFACE_API_TOKEN ou FINDFACE_EXTERNAL_TOKEN (token de API)")
	}

	eventsToken := os.Getenv("FINDFACE_EVENTS_TOKEN")
	if eventsToken == "" {
		return nil, fmt.Errorf("FINDFACE_EVENTS_TOKEN não definido (token de criação de eventos do external detector)")
	}

	cameraStr := os.Getenv("FINDFACE_CAMERA_ID")
	if cameraStr == "" {
		return nil, fmt.Errorf("FINDFACE_CAMERA_ID não definido")
	}
	cameraID, err := strconv.Atoi(cameraStr)
	if err != nil {
		return nil, fmt.Errorf("FINDFACE_CAMERA_ID inválido (%q): %w", cameraStr, err)
	}

	nameField := os.Getenv("FINDFACE_NAME_FIELD") // opcional
	return New(baseURL, apiToken, eventsToken, cameraID, nameField), nil
}

// CreateFaceEventFromFile envia uma imagem para /events/faces/add/.
//
// - Header: Authorization: Token <APIToken>
// - Form-data:
//     token       = <EventsToken>        (token do external detector)
//     fullframe   = arquivo de imagem
//     camera      = id da câmera
//     mf_selector = biggest
//     timestamp   = agora (UTC, RFC3339)
func (c *Client) CreateFaceEventFromFile(ctx context.Context, imagePath string) (*CreateFaceEventResponse, error) {
	file, err := os.Open(imagePath)
	if err != nil {
		return nil, fmt.Errorf("erro ao abrir arquivo %s: %w", imagePath, err)
	}
	defer file.Close()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Campo de arquivo: fullframe
	fw, err := writer.CreateFormFile("fullframe", filepath.Base(imagePath))
	if err != nil {
		return nil, fmt.Errorf("erro ao criar part fullframe: %w", err)
	}
	if _, err := io.Copy(fw, file); err != nil {
		return nil, fmt.Errorf("erro ao copiar arquivo para multipart: %w", err)
	}

	// token do external detector (campo "token" do form)
	if err := writer.WriteField("token", c.EventsToken); err != nil {
		return nil, fmt.Errorf("erro ao escrever campo token: %w", err)
	}

	// ID da câmera lógica
	if c.CameraID != 0 {
		if err := writer.WriteField("camera", strconv.Itoa(c.CameraID)); err != nil {
			return nil, fmt.Errorf("erro ao escrever campo camera: %w", err)
		}
	}

	// Seleciona o maior rosto da imagem
	_ = writer.WriteField("mf_selector", "biggest")

	// Timestamp do evento
	_ = writer.WriteField("timestamp", time.Now().UTC().Format(time.RFC3339))

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("erro ao fechar multipart writer: %w", err)
	}

	// Monta requisição
	urlReq := c.BaseURL + "/events/faces/add/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlReq, &buf)
	if err != nil {
		return nil, fmt.Errorf("erro ao criar request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	// Aqui vai o token de API (global)
	req.Header.Set("Authorization", "Token "+c.APIToken)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("erro ao chamar faces/add: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("erro ao ler resposta faces/add: %w", err)
	}

	// Checa status HTTP
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("faces/add status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return parseCreateFaceEventResponse(bodyBytes), nil
}

// CreateFaceEventFromBytes é igual ao de arquivo, mas recebe os bytes da imagem.
func (c *Client) CreateFaceEventFromBytes(ctx context.Context, img []byte, filename string) (*CreateFaceEventResponse, error) {
	if len(img) == 0 {
		return nil, fmt.Errorf("imagem vazia")
	}
	if filename == "" {
		filename = "snapshot.jpg"
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Arquivo fullframe
	fw, err := writer.CreateFormFile("fullframe", filename)
	if err != nil {
		return nil, fmt.Errorf("erro ao criar campo fullframe: %w", err)
	}
	if _, err := fw.Write(img); err != nil {
		return nil, fmt.Errorf("erro ao escrever bytes da imagem: %w", err)
	}

	// token do external detector
	if err := writer.WriteField("token", c.EventsToken); err != nil {
		return nil, fmt.Errorf("erro ao escrever campo token: %w", err)
	}

	// ID da câmera lógica
	if c.CameraID != 0 {
		if err := writer.WriteField("camera", strconv.Itoa(c.CameraID)); err != nil {
			return nil, fmt.Errorf("erro ao escrever campo camera: %w", err)
		}
	}

	// maior rosto
	_ = writer.WriteField("mf_selector", "biggest")

	// Timestamp
	_ = writer.WriteField("timestamp", time.Now().UTC().Format(time.RFC3339))

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("erro ao fechar multipart writer: %w", err)
	}

	urlReq := c.BaseURL + "/events/faces/add/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlReq, &buf)
	if err != nil {
		return nil, fmt.Errorf("erro ao criar request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Token "+c.APIToken)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("erro ao chamar faces/add: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("erro ao ler resposta faces/add: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("faces/add status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return parseCreateFaceEventResponse(bodyBytes), nil
}

// parseia o JSON da resposta de faces/add, sem exigir campo "id".
func parseCreateFaceEventResponse(bodyBytes []byte) *CreateFaceEventResponse {
	if len(bodyBytes) == 0 {
		return &CreateFaceEventResponse{}
	}

	var anyJSON interface{}
	if err := json.Unmarshal(bodyBytes, &anyJSON); err != nil {
		// resposta não-JSON, mas sucesso HTTP -> devolvemos corpo bruto mesmo assim
		return &CreateFaceEventResponse{
			EventID: "",
			RawJSON: bodyBytes,
		}
	}

	eventID := extractID(anyJSON)

	return &CreateFaceEventResponse{
		EventID: eventID,
		RawJSON: bodyBytes,
	}
}

// GetFaceEvent busca os detalhes de um evento específico em /events/faces/?id_in=<id>&limit=1.
func (c *Client) GetFaceEvent(ctx context.Context, eventID string) (*FaceEvent, error) {
	if strings.TrimSpace(eventID) == "" {
		return nil, fmt.Errorf("eventID vazio")
	}

	u, err := url.Parse(c.BaseURL + "/events/faces/")
	if err != nil {
		return nil, fmt.Errorf("url inválida base: %w", err)
	}

	q := u.Query()
	// alguns FindFace aceitam id_in, outros id — aqui usamos id_in,
	// que foi o que você mandou na doc.
	q.Set("id_in", eventID)
	q.Set("limit", "1")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("erro ao criar request GetFaceEvent: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.APIToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("erro ao chamar GetFaceEvent: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("erro ao ler resposta GetFaceEvent: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GetFaceEvent status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var envelope struct {
		Count   int         `json:"count"`
		Results []FaceEvent `json:"results"`
	}
	if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
		return nil, fmt.Errorf("erro ao parsear JSON GetFaceEvent: %w (body=%s)", err, string(bodyBytes))
	}

	if len(envelope.Results) == 0 {
		return nil, fmt.Errorf("GetFaceEvent: nenhum evento encontrado com id_in=%s (body=%s)", eventID, string(bodyBytes))
	}

	return &envelope.Results[0], nil
}

// GetCard busca um human card (pessoa) pelo ID.
// Endpoint: GET /cards/humans/{id}/
func (c *Client) GetCard(ctx context.Context, cardID int) (*Card, error) {
	urlReq := fmt.Sprintf("%s/cards/humans/%d/", c.BaseURL, cardID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlReq, nil)
	if err != nil {
		return nil, fmt.Errorf("erro ao criar request GetCard: %w", err)
	}
	// aqui vai o token de API (FINDFACE_API_TOKEN ou FINDFACE_EXTERNAL_TOKEN)
	req.Header.Set("Authorization", "Token "+c.APIToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("erro ao chamar GetCard: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("erro ao ler resposta GetCard: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GetCard status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var card Card
	if err := json.Unmarshal(bodyBytes, &card); err != nil {
		return nil, fmt.Errorf("erro ao parsear JSON GetCard: %w (body=%s)", err, string(bodyBytes))
	}

	return &card, nil
}

// GetCardName retorna um "nome amigável" para o card,
// usando (nesta ordem):
// 1) card.Name
// 2) card.Features[c.NameField] (ex: features.name)
// 3) fallbacks comuns em Features/Meta.
func (c *Client) GetCardName(card *Card) string {
	if card == nil {
		return ""
	}

	// 1) campo direto no root: "name"
	if card.Name != nil && strings.TrimSpace(*card.Name) != "" {
		return strings.TrimSpace(*card.Name)
	}

	// 2) features[c.NameField]
	if card.Features != nil && c.NameField != "" {
		if raw, ok := card.Features[c.NameField]; ok {
			if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}

	// 3) fallbacks em features/meta
	candidates := []string{"name", "full_name", "fullname", "nome"}
	for _, key := range candidates {
		if card.Features != nil {
			if raw, ok := card.Features[key]; ok {
				if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		if card.Meta != nil {
			if raw, ok := card.Meta[key]; ok {
				if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}

	// 4) fallback final: ID
	return fmt.Sprintf("card-%d", card.ID)
}


// GetCardPhotoURL tenta extrair uma URL de foto do card,
// olhando primeiro por chaves "fortes" (photo, avatar, image, picture, thumbnail)
// e depois por qualquer campo string que pareça uma URL (começa com http).
func (c *Client) GetCardPhotoURL(card *Card) string {
    if card == nil {
        return ""
    }

    // Candidatos em ordem de prioridade
    candidates := []string{
        "photo_url", "photo", "avatar", "image", "picture",
        "thumbnail", "thumb", "foto", "imagem",
    }

    // 1) Features[candidate]
    for _, key := range candidates {
        if card.Features != nil {
            if raw, ok := card.Features[key]; ok {
                if s, ok := raw.(string); ok && strings.HasPrefix(strings.TrimSpace(s), "http") {
                    return strings.TrimSpace(s)
                }
            }
        }
        if card.Meta != nil {
            if raw, ok := card.Meta[key]; ok {
                if s, ok := raw.(string); ok && strings.HasPrefix(strings.TrimSpace(s), "http") {
                    return strings.TrimSpace(s)
                }
            }
        }
    }

    // 2) fallback: qualquer campo string que pareça URL em Features/Meta
    for _, kv := range []map[string]interface{}{card.Features, card.Meta} {
        for _, raw := range kv {
            if s, ok := raw.(string); ok && strings.HasPrefix(strings.TrimSpace(s), "http") {
                return strings.TrimSpace(s)
            }
        }
    }

    return ""
}


// extractID tenta achar um campo "id" em formatos comuns
// e também o primeiro id em "events":[ "...", ... ] (formato que você recebeu).
func extractID(v interface{}) string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}

	// 1) id direto
	if raw, ok := m["id"]; ok {
		if s := toStringID(raw); s != "" {
			return s
		}
	}

	// 2) event_id
	if raw, ok := m["event_id"]; ok {
		if s := toStringID(raw); s != "" {
			return s
		}
	}

	// 3) results[0].id
	if raw, ok := m["results"]; ok {
		if arr, ok := raw.([]interface{}); ok && len(arr) > 0 {
			if first, ok := arr[0].(map[string]interface{}); ok {
				if rawID, ok := first["id"]; ok {
					if s := toStringID(rawID); s != "" {
						return s
					}
				}
			}
		}
	}

	// 4) events[0]
	if raw, ok := m["events"]; ok {
		if arr, ok := raw.([]interface{}); ok && len(arr) > 0 {
			if s := toStringID(arr[0]); s != "" {
				return s
			}
		}
	}

	return ""
}

// toStringID converte um valor num id em string, se possível.
func toStringID(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}


// GetFaceObjectForCard busca um objeto de face (foto) ligado a um card.
// Endpoint: GET /objects/faces/?card=<id>&limit=1&ordering=-created_date
func (c *Client) GetFaceObjectForCard(ctx context.Context, cardID int) (*FaceObject, error) {
    u, err := url.Parse(c.BaseURL + "/objects/faces/")
    if err != nil {
        return nil, fmt.Errorf("url inválida base objects/faces: %w", err)
    }

    q := u.Query()
    q.Set("card", strconv.Itoa(cardID))
    q.Set("limit", "1")
    q.Set("ordering", "-created_date")
    u.RawQuery = q.Encode()

    req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
    if err != nil {
        return nil, fmt.Errorf("erro ao criar request GetFaceObjectForCard: %w", err)
    }
    req.Header.Set("Authorization", "Token "+c.APIToken)
    req.Header.Set("Accept", "application/json")

    resp, err := c.HTTP.Do(req)
    if err != nil {
        return nil, fmt.Errorf("erro ao chamar GetFaceObjectForCard: %w", err)
    }
    defer resp.Body.Close()

    bodyBytes, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, fmt.Errorf("erro ao ler resposta GetFaceObjectForCard: %w", err)
    }

    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("GetFaceObjectForCard status %d: %s", resp.StatusCode, string(bodyBytes))
    }

    var envelope struct {
        Count   int          `json:"count"`
        Results []FaceObject `json:"results"`
    }
    if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
        return nil, fmt.Errorf("erro ao parsear JSON GetFaceObjectForCard: %w (body=%s)", err, string(bodyBytes))
    }

    if len(envelope.Results) == 0 {
        return nil, nil // sem objeto ligado a esse card
    }

    return &envelope.Results[0], nil
}