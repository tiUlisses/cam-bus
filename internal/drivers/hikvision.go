// internal/drivers/hikvision.go
package drivers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"math/rand"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sua-org/cam-bus/internal/core"
	"github.com/sua-org/cam-bus/internal/storage"
)

type HikvisionDriver struct {
	info   core.CameraInfo
	client *http.Client
}

func NewHikvisionDriver(info core.CameraInfo) (CameraDriver, error) {
    var httpClient *http.Client

    if info.UseTLS {
        // SEM controle por env: sempre ignora cert quando UseTLS=true
        tr := &http.Transport{
            TLSClientConfig: &tls.Config{
                InsecureSkipVerify: true, //nolint:gosec - uso consciente em rede interna
            },
        }
        httpClient = &http.Client{
            Timeout:   0,
            Transport: tr,
        }
        log.Printf("[hikvision] TLS inseguro (sempre) habilitado para %s (%s)",
            info.Name, info.IP)
    } else {
        httpClient = &http.Client{
            Timeout: 0,
        }
    }

    d := &HikvisionDriver{
        info:   info,
        client: httpClient,
    }
    return d, nil
}


func init() {
	// registra Hikvision para qualquer modelo: "hikvision:any"
	RegisterDriver("hikvision", "any", func(info core.CameraInfo) (CameraDriver, error) {
		return NewHikvisionDriver(info)
	})
}

// Run abre o subscribeEvent e fica recebendo eventos (faceCapture, etc.).
func (d *HikvisionDriver) Run(ctx context.Context, events chan<- core.AnalyticEvent) error {
	log.Printf("[hikvision] starting driver for %s (%s)", d.info.Name, d.info.IP)

	// Laço de reconexão em caso de erro
	for {
		if err := d.runOnce(ctx, events); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[hikvision] error for %s: %v, retrying in 5s", d.info.Name, err)
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

func (d *HikvisionDriver) runOnce(ctx context.Context, events chan<- core.AnalyticEvent) error {
	// Monta URL base
	scheme := "http"
	if d.info.UseTLS {
		scheme = "https"
	}
	
	host := d.info.IP
	if d.info.Port != 0 {
		host = fmt.Sprintf("%s:%d", host, d.info.Port)
	}
	
	baseURL := fmt.Sprintf("%s://%s", scheme, host)

	// Opcional: consultar capabilities (pode ser útil, mas não é obrigatório)
	// _, _ = d.doDigest(ctx, http.MethodGet, baseURL+"/ISAPI/Event/notification/subscribeEventCap", nil, "")

	// Faz subscribe para faceCapture/analytics em formato JSON
	subURL := baseURL + "/ISAPI/Event/notification/subscribeEvent"
	body := d.buildSubscribeEventXML()

	reqBody := bytes.NewReader(body)
	resp, err := d.doDigest(ctx, http.MethodPost, subURL, reqBody, "application/xml")
	if err != nil {
		return fmt.Errorf("subscribeEvent error: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return fmt.Errorf("subscribeEvent status %d: %s", resp.StatusCode, string(b))
	}

	log.Printf("[hikvision] subscribed event stream for %s (%s)", d.info.Name, d.info.IP)

	// Lê o cabeçalho Content-Type pra pegar o boundary
	ct := resp.Header.Get("Content-Type")
	mediatype, params, err := mime.ParseMediaType(ct)
	if err != nil {
		resp.Body.Close()
		return fmt.Errorf("invalid Content-Type %q: %w", ct, err)
	}
	if !strings.HasPrefix(mediatype, "multipart/") {
		// Em algumas versões, pode vir "application/xml" contínuo; aqui assumimos multipart.
		resp.Body.Close()
		return fmt.Errorf("unexpected media type: %s", mediatype)
	}

	boundary := params["boundary"]
	if boundary == "" {
		resp.Body.Close()
		return fmt.Errorf("no boundary in Content-Type: %s", ct)
	}

	mr := multipart.NewReader(resp.Body, boundary)

	// pendingEvent: guardamos o evento textual até chegar a imagem.
	var pendingEvent *core.AnalyticEvent

	for {
		part, err := mr.NextPart()
		if err != nil {
			if err == io.EOF {
				resp.Body.Close()
				return fmt.Errorf("stream ended")
			}
			resp.Body.Close()
			return fmt.Errorf("error reading part: %w", err)
		}

		pCT := part.Header.Get("Content-Type")

		if strings.HasPrefix(pCT, "application/json") {
			// Evento em JSON
			data, err := io.ReadAll(part)
			if err != nil {
				log.Printf("[hikvision] error reading json part: %v", err)
				continue
			}

			evt, err := d.parseJSONEvent(data)
			if err != nil {
				log.Printf("[hikvision] json parse error: %v; raw=%s", err, string(data))
				continue
			}
			pendingEvent = evt
			continue
		}

		if strings.HasPrefix(pCT, "application/xml") || strings.HasPrefix(pCT, "text/xml") {
			// Evento em XML (não é o foco, mas podemos tentar extrair infos básicas)
			data, err := io.ReadAll(part)
			if err != nil {
				log.Printf("[hikvision] error reading xml part: %v", err)
				continue
			}
			evt, err := d.parseXMLEvent(data)
			if err != nil {
				log.Printf("[hikvision] xml parse error: %v", err)
				continue
			}
			pendingEvent = evt
			continue
		}

		if strings.HasPrefix(pCT, "image/") {
			imgBytes, err := io.ReadAll(part)
			if err != nil {
				log.Printf("[hikvision] error reading image part: %v", err)
				continue
			}

			if pendingEvent != nil {
				// Salva em MinIO, se disponível
				if storage.DefaultStore != nil {
					ctxUp, cancelUp := context.WithTimeout(ctx, 5*time.Second)
					url, err := storage.DefaultStore.SaveSnapshot(ctxUp, d.buildSnapshotKey(pendingEvent), imgBytes, pCT)
					cancelUp()
					if err != nil {
						log.Printf("[hikvision] erro ao salvar snapshot no MinIO: %v", err)
					} else {
						pendingEvent.SnapshotURL = url
					}
				}

				// Sempre guarda base64 para o faceengine poder usar,
				// mesmo que o MinIO esteja privado.
				pendingEvent.SnapshotB64 = base64.StdEncoding.EncodeToString(imgBytes)

				// Envia evento
				select {
				case events <- *pendingEvent:
				case <-ctx.Done():
					part.Close()
					resp.Body.Close()
					return nil
				}

				pendingEvent = nil
			} else {
				log.Printf("[hikvision] image part sem evento pendente, descartando")
			}

			continue
		}

		// Outros tipos: apenas descarta
		_ = part.Close()
	}
}

// buildSubscribeEventXML monta o XML de subscribeEvent
// baseado na lista de analytics vinda do /info (CameraInfo.Analytics).
// Se não vier nada válido, cai no fallback: faceCapture.
func (d *HikvisionDriver) buildSubscribeEventXML() []byte {
	// 1) Monta lista de eventTypes a partir do /info
	var selected []string

	if len(d.info.Analytics) > 0 {
		for _, a := range d.info.Analytics {
			name := strings.TrimSpace(a)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if _, ok := core.HikvisionEventTypeSet[key]; ok {
				selected = append(selected, name)
			} else {
				log.Printf(
					"[hikvision] camera %s: analytics '%s' não é suportado, ignorando",
					d.info.DeviceID, name,
				)
			}
		}
	}

	// 2) Se nada configurado ou tudo inválido, fallback para faceCapture
	if len(selected) == 0 {
		selected = []string{"faceCapture"}
		log.Printf(
			"[hikvision] camera %s: nenhum analytics válido no /info, usando fallback faceCapture",
			d.info.DeviceID,
		)
	}

	// 3) Monta XML com eventMode=list e EventList com todos os tipos
	var b strings.Builder
	b.WriteString(`<SubscribeEvent xmlns="http://www.isapi.org/ver20/XMLSchema">`)
	b.WriteString(`<format>json</format>`)
	b.WriteString(`<heartbeat>30</heartbeat>`)
	b.WriteString(`<eventMode>list</eventMode>`)
	b.WriteString(`<EventList>`)

	for _, t := range selected {
		b.WriteString(`<Event><type>`)
		b.WriteString(t)
		b.WriteString(`</type><channels>1</channels></Event>`)
	}

	b.WriteString(`</EventList>`)
	b.WriteString(`</SubscribeEvent>`)

	return []byte(b.String())
}

// ----------------------------------
// Digest Auth helper
// ----------------------------------

func (d *HikvisionDriver) doDigest(
	ctx context.Context,
	method, rawURL string,
	body io.Reader,
	contentType string,
) (*http.Response, error) {
	// 1ª tentativa sem Authorization, só pra pegar WWW-Authenticate
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

	// 401: parse WWW-Authenticate
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

	// Monta segunda requisição com Authorization Digest
	// Precisamos recriar o body, pois já foi consumido na 1ª tentativa.
	var bodyBytes []byte
	if body != nil {
		if rb, ok := body.(*bytes.Reader); ok {
			// reader original era bytes.Reader
			rb.Seek(0, io.SeekStart)
			bodyBytes, _ = io.ReadAll(rb)
		} else if b, ok := body.(*bytes.Buffer); ok {
			bodyBytes = b.Bytes()
		} else {
			// sem como reaproveitar: consideramos que as chamadas que usam body
			// já estão passando bytes.Reader/buffer (SubscribeEvent, etc.)
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

type digestChallenge struct {
	Realm string
	Nonce string
	Qop   string
}

var digestRx = regexp.MustCompile(`(\w+)="([^"]+)"`)

func parseDigestAuthHeader(h string) (*digestChallenge, error) {
	if !strings.HasPrefix(strings.ToLower(h), "digest ") {
		return nil, fmt.Errorf("WWW-Authenticate não é Digest: %s", h)
	}
	h = strings.TrimSpace(h[len("Digest "):])
	m := digestRx.FindAllStringSubmatch(h, -1)
	res := &digestChallenge{}
	for _, kv := range m {
		if len(kv) != 3 {
			continue
		}
		k := strings.ToLower(kv[1])
		v := kv[2]
		switch k {
		case "realm":
			res.Realm = v
		case "nonce":
			res.Nonce = v
		case "qop":
			res.Qop = v
		}
	}
	if res.Realm == "" || res.Nonce == "" {
		return nil, fmt.Errorf("realm/nonce ausentes em WWW-Authenticate: %s", h)
	}
	if res.Qop == "" {
		res.Qop = "auth"
	}
	return res, nil
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		// fallback fraco, mas suficiente aqui
		for i := range b {
			b[i] = byte(rand.Intn(256))
		}
	}
	return hex.EncodeToString(b)
}

// ----------------------------------
// Parse de eventos JSON/XML
// ----------------------------------

func (d *HikvisionDriver) parseJSONEvent(data []byte) (*core.AnalyticEvent, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	eventType := getString(raw, "eventType")
	analytic := eventType
	if analytic == "" {
		analytic = "unknown"
	}

	meta := map[string]interface{}{
		"eventType":        eventType,
		"eventDescription": getString(raw, "eventDescription"),
		"eventState":       getString(raw, "eventState"),
		"channelID":        getNumber(raw, "channelID"),
		"channelName":      getString(raw, "channelName"),
	}

	// faceCapture: extrai informações da lista faceCapture[]
	if eventType == "faceCapture" {
		bestScore := 0.0
		facesCount := 0

		if fcRaw, ok := raw["faceCapture"]; ok {
			if arr, ok2 := fcRaw.([]interface{}); ok2 {
				for _, item := range arr {
					obj, ok3 := item.(map[string]interface{})
					if !ok3 {
						continue
					}
					if facesRaw, ok4 := obj["faces"]; ok4 {
						if facesArr, ok5 := facesRaw.([]interface{}); ok5 {
							facesCount += len(facesArr)
							for _, f := range facesArr {
								fObj, ok6 := f.(map[string]interface{})
								if !ok6 {
									continue
								}
								if sc, ok7 := fObj["faceScore"].(float64); ok7 {
									if sc > bestScore {
										bestScore = sc
									}
								}
							}
						}
					}
				}
			}
		}

		meta["facesCount"] = facesCount
		meta["bestScore"] = bestScore
	}

	tsStr := getString(raw, "dateTime")
	var ts time.Time
	if tsStr != "" {
		t, err := time.Parse(time.RFC3339, tsStr)
		if err == nil {
			ts = t.UTC()
		}
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	evt := &core.AnalyticEvent{
		Timestamp:    ts,
		EventID:      d.buildJSONEventID(raw),
		CameraIP:     d.info.IP,
		CameraName:   d.info.Name,
		AnalyticType: analytic,
		Meta:         meta,

		Tenant:     d.info.Tenant,
		Building:   d.info.Building,
		Floor:      d.info.Floor,
		DeviceType: d.info.DeviceType,
		DeviceID:   d.info.DeviceID,
	}

	return evt, nil
}

func (d *HikvisionDriver) parseXMLEvent(data []byte) (*core.AnalyticEvent, error) {
	type EventNotificationAlert struct {
		XMLName          xml.Name `xml:"EventNotificationAlert"`
		EventType        string   `xml:"eventType"`
		EventDescription string   `xml:"eventDescription"`
		EventState       string   `xml:"eventState"`
		ChannelID        int      `xml:"channelID"`
		ChannelName      string   `xml:"channelName"`
		DateTime         string   `xml:"dateTime"`
	}
	type Wrapper struct {
		XMLName xml.Name `xml:"EventNotificationAlert"`
		EventNotificationAlert
	}

	// Alguns firmwares usam namespace, então vamos remover namespace simples
	cleaned := stripXMLNamespace(data)

	var alert EventNotificationAlert
	if err := xml.Unmarshal(cleaned, &alert); err != nil {
		return nil, err
	}

	meta := map[string]interface{}{
		"eventType":        alert.EventType,
		"eventDescription": alert.EventDescription,
		"eventState":       alert.EventState,
		"channelID":        alert.ChannelID,
		"channelName":      alert.ChannelName,
	}

	analytic := alert.EventType
	if analytic == "" {
		analytic = "unknown"
	}

	var ts time.Time
	if alert.DateTime != "" {
		t, err := time.Parse(time.RFC3339, alert.DateTime)
		if err == nil {
			ts = t.UTC()
		}
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	evt := &core.AnalyticEvent{
		Timestamp:    ts,
		EventID:      fmt.Sprintf("xml-%d", ts.UnixNano()),
		CameraIP:     d.info.IP,
		CameraName:   d.info.Name,
		AnalyticType: analytic,
		Meta:         meta,

		Tenant:     d.info.Tenant,
		Building:   d.info.Building,
		Floor:      d.info.Floor,
		DeviceType: d.info.DeviceType,
		DeviceID:   d.info.DeviceID,
	}

	return evt, nil
}

// ----------------------------------
// Helpers diversos
// ----------------------------------

func (d *HikvisionDriver) buildSnapshotKey(evt *core.AnalyticEvent) string {
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

func (d *HikvisionDriver) buildJSONEventID(raw map[string]interface{}) string {
	// tenta alguns campos; se não tiver, gera um ID pseudo-único
	if v, ok := raw["uid"].(string); ok && v != "" {
		return v
	}
	if v, ok := raw["eventID"].(string); ok && v != "" {
		return v
	}
	return fmt.Sprintf("json-%d", time.Now().UnixNano())
}

func getString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok2 := v.(string); ok2 {
				return s
			}
		}
	}
	return ""
}

func getNumber(m map[string]interface{}, key string) interface{} {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			return x
		case int:
			return x
		case int64:
			return x
		}
	}
	return nil
}

func safePath(v, def string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		v = def
	}
	v = strings.ToLower(v)
	v = strings.ReplaceAll(v, " ", "_")
	v = strings.ReplaceAll(v, "/", "-")
	return v
}

// remove namespaces simples tipo <ns:Tag> -> <Tag>
func stripXMLNamespace(b []byte) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 0, len(b)), len(b))
	var out bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		line = regexp.MustCompile(`</?\w+:`).ReplaceAllString(line, "<")
		out.WriteString(line)
	}
	if out.Len() == 0 {
		return b
	}
	return out.Bytes()
}
