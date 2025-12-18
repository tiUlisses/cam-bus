// internal/drivers/dahua.go
package drivers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sua-org/cam-bus/internal/core"
	"github.com/sua-org/cam-bus/internal/storage"
)

type DahuaDriver struct {
	info          core.CameraInfo
	client        *http.Client
	statusHandler func(StatusUpdate)
}

func NewDahuaDriver(info core.CameraInfo) (CameraDriver, error) {
	var httpClient *http.Client

	if info.UseTLS {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // rede interna, cert inválido
			},
		}
		httpClient = &http.Client{
			Timeout:   0,
			Transport: tr,
		}
		log.Printf("[dahua] TLS inseguro habilitado para %s (%s)", info.Name, info.IP)
	} else {
		httpClient = &http.Client{
			Timeout: 0,
		}
	}

	return &DahuaDriver{
		info:   info,
		client: httpClient,
	}, nil
}

// SetStatusHandler registra callback para mudanças de status de conexão.
func (d *DahuaDriver) SetStatusHandler(fn func(StatusUpdate)) {
	d.statusHandler = fn
}

// ActiveAnalytics retorna a lista efetiva de analytics assinados para a câmera.
func (d *DahuaDriver) ActiveAnalytics() []string {
	return d.selectedEventCodes()
}

func (d *DahuaDriver) notifyStatus(update StatusUpdate) {
	if d.statusHandler != nil {
		d.statusHandler(update)
	}
}

func init() {
	// fabricante "Dahua", modelo "any"
	RegisterDriver("dahua", "any", func(info core.CameraInfo) (CameraDriver, error) {
		return NewDahuaDriver(info)
	})
}

// selectedEventCodes decide quais códigos de evento Dahua vamos assinar,
// baseado em info.Analytics e na lista core.DahuaEventTypes.
//
// Regras:
// - Se info.Analytics tiver "ALL" ou "*" => usa todos core.DahuaEventTypes.
// - Senão, pega só os que existem em core.DahuaEventTypeSet.
// - Se nada válido => fallback em ["FaceDetection"] (comportamento atual).
func (d *DahuaDriver) selectedEventCodes() []string {
	var selected []string
	allRequested := false

	for _, a := range d.info.Analytics {
		name := strings.TrimSpace(a)
		if name == "" {
			continue
		}

		if strings.EqualFold(name, "all") || name == "*" {
			allRequested = true
			break
		}

		key := strings.ToLower(name)
		if _, ok := core.DahuaEventTypeSet[key]; ok {
			selected = append(selected, name)
		} else {
			log.Printf(
				"[dahua] camera %s: analytics '%s' não é suportado, ignorando",
				d.info.DeviceID, name,
			)
		}
	}

	if allRequested {
		log.Printf("[dahua] camera %s: usando TODOS os DahuaEventTypes (ALL/* no /info)",
			d.info.DeviceID)
		return core.DahuaEventTypes
	}

	if len(selected) == 0 {
		log.Printf(
			"[dahua] camera %s: nenhum analytics válido no /info, usando fallback FaceDetection",
			d.info.DeviceID,
		)
		return []string{"FaceDetection"}
	}

	return selected
}

func (d *DahuaDriver) Run(ctx context.Context, events chan<- core.AnalyticEvent) error {
	log.Printf("[dahua] starting driver for %s (%s)", d.info.Name, d.info.IP)

	for {
		if err := d.runOnce(ctx, events); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[dahua] error for %s: %v, retrying in 5s", d.info.Name, err)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return nil
			}
		} else {
			return nil
		}
	}
}

func (d *DahuaDriver) runOnce(ctx context.Context, events chan<- core.AnalyticEvent) error {
	scheme := "http"
	if d.info.UseTLS {
		scheme = "https"
	}
	host := d.info.IP
	if d.info.Port != 0 {
		host = fmt.Sprintf("%s:%d", host, d.info.Port)
	}

	// Define quais códigos de evento vamos assinar, com base no /info.
	codes := d.selectedEventCodes()
	codesStr := strings.Join(codes, ",")
	d.notifyStatus(StatusUpdate{State: ConnectionStateConnecting, Reason: "abrindo stream"})

	// Mapa para validar somente os códigos que vieram do MQTT (/info).
	allowedCodes := make(map[string]struct{}, len(codes))
	for _, c := range codes {
		lc := strings.ToLower(strings.TrimSpace(c))
		if lc == "" {
			continue
		}
		allowedCodes[lc] = struct{}{}
	}

	// Stream de eventos Dahua: agora com múltiplos códigos.
	evtURL := fmt.Sprintf(
		"%s://%s/cgi-bin/eventManager.cgi?action=attach&codes=[%s]&heartbeat=5",
		scheme,
		host,
		codesStr,
	)

	resp, err := d.doDigest(ctx, http.MethodGet, evtURL, nil, "")
	if err != nil {
		d.notifyStatus(StatusUpdate{State: ConnectionStateNotEstablished, Reason: err.Error()})
		return fmt.Errorf("eventManager attach error: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		d.notifyStatus(StatusUpdate{State: ConnectionStateNotEstablished, Reason: string(b)})
		return fmt.Errorf("eventManager status %d: %s", resp.StatusCode, string(b))
	}

	ct := resp.Header.Get("Content-Type")
	mediatype, params, err := mime.ParseMediaType(ct)
	if err != nil {
		resp.Body.Close()
		d.notifyStatus(StatusUpdate{State: ConnectionStateNotEstablished, Reason: err.Error()})
		return fmt.Errorf("invalid Content-Type %q: %w", ct, err)
	}
	if !strings.HasPrefix(mediatype, "multipart/") {
		resp.Body.Close()
		d.notifyStatus(StatusUpdate{State: ConnectionStateNotEstablished, Reason: "unexpected media type"})
		return fmt.Errorf("unexpected media type: %s", mediatype)
	}
	boundary := params["boundary"]
	if boundary == "" {
		resp.Body.Close()
		d.notifyStatus(StatusUpdate{State: ConnectionStateNotEstablished, Reason: "missing boundary"})
		return fmt.Errorf("no boundary in Content-Type: %s", ct)
	}

	d.notifyStatus(StatusUpdate{
		State:  ConnectionStateOnline,
		Reason: fmt.Sprintf("subscribed to [%s]", codesStr),
	})

	mr := multipart.NewReader(resp.Body, boundary)

	for {
		part, err := mr.NextPart()
		if err != nil {
			if err == io.EOF {
				resp.Body.Close()
				d.notifyStatus(StatusUpdate{State: ConnectionStateOffline, Reason: "stream ended"})
				return fmt.Errorf("stream ended")
			}
			resp.Body.Close()
			d.notifyStatus(StatusUpdate{State: ConnectionStateOffline, Reason: err.Error()})
			return fmt.Errorf("error reading part: %w", err)
		}

		pCT := part.Header.Get("Content-Type")
		if pCT == "" || strings.HasPrefix(pCT, "text/plain") {
			data, err := io.ReadAll(part)
			_ = part.Close()
			if err != nil {
				log.Printf("[dahua] error reading text part: %v", err)
				continue
			}

			evt, snapshotBytes, snapshotCT, err := d.parseEventAndSnapshot(ctx, data, allowedCodes)
			if err != nil {
				log.Printf("[dahua] parseEvent error: %v; raw=%s", err, string(data))
				continue
			}
			if evt == nil {
				// evento ignorado (código não permitido, action != Start, etc.)
				continue
			}

			// Se conseguimos snapshot, salva no MinIO + base64
			if len(snapshotBytes) > 0 {
				if storage.DefaultStore != nil {
					ctxUp, cancelUp := context.WithTimeout(ctx, 5*time.Second)
					url, err := storage.DefaultStore.SaveSnapshot(ctxUp, d.buildSnapshotKey(evt), snapshotBytes, snapshotCT)
					cancelUp()
					if err != nil {
						log.Printf("[dahua] erro ao salvar snapshot no MinIO: %v", err)
					} else {
						evt.SnapshotURL = url
					}
				}
				evt.SnapshotB64 = base64.StdEncoding.EncodeToString(snapshotBytes)
			}

			select {
			case events <- *evt:
			case <-ctx.Done():
				resp.Body.Close()
				return nil
			}

			continue
		}

		// outros tipos: ignora
		_ = part.Close()
	}
}

// parseEventAndSnapshot parseia o texto do evento Dahua e, se o código
// estiver na lista permitida pelo /info (allowedCodes), gera um AnalyticEvent
// e tenta baixar um snapshot único da câmera.
//
// Mantém o comportamento original pra FaceDetection (Start + snapshot),
// e extende para outros analíticos (CrossLineDetection, CrossRegionDetection, etc.),
// também capturando snapshot.
func (d *DahuaDriver) parseEventAndSnapshot(
	ctx context.Context,
	data []byte,
	allowedCodes map[string]struct{},
) (*core.AnalyticEvent, []byte, string, error) {
	body := string(data)

	// Formato típico: "Code=FaceDetection;action=Start;index=0;..."
	code := extractKV(body, "Code")
	if code == "" {
		code = extractKV(body, "code")
	}
	if code == "" {
		// não conseguimos identificar código -> ignora
		return nil, nil, "", nil
	}

	codeNorm := strings.ToLower(strings.TrimSpace(code))
	if _, ok := allowedCodes[codeNorm]; !ok {
		// código não está na lista que veio do MQTT (/info) -> ignora
		return nil, nil, "", nil
	}

	// Só processamos action=Start (mantém comportamento original e evita flood)
	action := extractKV(body, "action")
	if action != "" && !strings.EqualFold(action, "Start") {
		return nil, nil, "", nil
	}

	ts := time.Now().UTC()

	meta := map[string]interface{}{
		"raw":    body,
		"code":   code,
		"action": action,
	}

	evt := &core.AnalyticEvent{
		Timestamp:    ts,
		EventID:      fmt.Sprintf("dahua-%d", ts.UnixNano()),
		CameraIP:     d.info.IP,
		CameraName:   d.info.Name,
		AnalyticType: code, // ex: "FaceDetection", "CrossLineDetection", etc.
		Meta:         meta,

		Tenant:     d.info.Tenant,
		Building:   d.info.Building,
		Floor:      d.info.Floor,
		DeviceType: d.info.DeviceType,
		DeviceID:   d.info.DeviceID,
	}

	// tenta pegar snapshot imediato (mesma rota já usada e validada)
	img, ctype, err := d.fetchSnapshot(ctx)
	if err != nil {
		log.Printf("[dahua] erro ao buscar snapshot: %v", err)
		// evento ainda é válido, só que sem imagem
		return evt, nil, "", nil
	}

	return evt, img, ctype, nil
}

// fetchSnapshot baixa um snapshot único da câmera Dahua.
// Em muitos modelos a rota é /cgi-bin/snapshot.cgi?channel=1.
// Se o teu for diferente, só ajusta essa URL.
func (d *DahuaDriver) fetchSnapshot(ctx context.Context) ([]byte, string, error) {
	scheme := "http"
	if d.info.UseTLS {
		scheme = "https"
	}
	host := d.info.IP
	if d.info.Port != 0 {
		host = fmt.Sprintf("%s:%d", host, d.info.Port)
	}

	snapURL := fmt.Sprintf("%s://%s/cgi-bin/snapshot.cgi?channel=1", scheme, host)

	resp, err := d.doDigest(ctx, http.MethodGet, snapURL, nil, "")
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("snapshot status %d: %s", resp.StatusCode, string(b))
	}

	ctype := resp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "image/jpeg"
	}

	img, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if len(img) == 0 {
		return nil, "", fmt.Errorf("snapshot vazio")
	}

	return img, ctype, nil
}

// buildSnapshotKey gera a chave para salvar snapshots Dahua no MinIO.
func (d *DahuaDriver) buildSnapshotKey(evt *core.AnalyticEvent) string {
	ts := evt.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	tenant := safePath(d.info.Tenant, "default")
	building := safePath(d.info.Building, "building")
	floor := safePath(d.info.Floor, "floor")
	dtype := safePath(d.info.DeviceType, "device")
	did := safePath(d.info.DeviceID, "id")
	analytic := safePath(evt.AnalyticType, "analytic")

	return fmt.Sprintf(
		"%s/%s/%s/%s/%s/%s/%04d/%02d/%02d/%s_%d.jpg",
		tenant, building, floor, dtype, did, analytic,
		ts.Year(), ts.Month(), ts.Day(),
		evt.EventID, ts.UnixNano(),
	)
}

// doDigest é igual ao da Hikvision, reaproveitando parseDigestAuthHeader/md5Hex/randomHex
// já definidos no pacote drivers (em hikvision.go).
func (d *DahuaDriver) doDigest(
	ctx context.Context,
	method, rawURL string,
	body io.Reader,
	contentType string,
) (*http.Response, error) {
	// 1ª tentativa sem Authorization
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Connection", "keep-alive")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	// 401 -> Digest
	authHeader := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()
	digest, err := parseDigestAuthHeader(authHeader)
	if err != nil {
		return nil, err
	}

	username := d.info.Username
	password := d.info.Password

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	// Recria body se necessário
	var bodyBytes []byte
	if body != nil {
		if rb, ok := body.(*bytes.Reader); ok {
			rb.Seek(0, io.SeekStart)
			bodyBytes, _ = io.ReadAll(rb)
		} else if b, ok := body.(*bytes.Buffer); ok {
			bodyBytes = b.Bytes()
		}
	}

	var body2 io.Reader
	if bodyBytes != nil {
		body2 = bytes.NewReader(bodyBytes)
	}

	nc := "00000001"
	cnonce := randomHex(16)
	ha1 := md5Hex(fmt.Sprintf("%s:%s:%s", username, digest.Realm, password))
	ha2 := md5Hex(fmt.Sprintf("%s:%s", method, u.RequestURI()))
	response := md5Hex(fmt.Sprintf("%s:%s:%s:%s:%s:%s",
		ha1, digest.Nonce, nc, cnonce, digest.Qop, ha2,
	))

	authValue := fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=MD5, response="%s", qop=%s, nc=%s, cnonce="%s"`,
		username,
		digest.Realm,
		digest.Nonce,
		u.RequestURI(),
		response,
		digest.Qop,
		nc,
		cnonce,
	)

	req2, err := http.NewRequestWithContext(ctx, method, rawURL, body2)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req2.Header.Set("Content-Type", contentType)
	}
	req2.Header.Set("Connection", "keep-alive")
	req2.Header.Set("Authorization", authValue)

	return d.client.Do(req2)
}

// extractKV pega "Key=Value" de um texto tosco do Dahua.
func extractKV(body, key string) string {
	key = key + "="
	idx := strings.Index(body, key)
	if idx == -1 {
		return ""
	}
	rest := body[idx+len(key):]
	// termina em ';' ou fim de string
	end := strings.Index(rest, ";")
	if end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}
