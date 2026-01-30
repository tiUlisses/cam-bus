// internal/supervisor/supervisor.go
package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/process"
	"github.com/sua-org/cam-bus/internal/core"
	"github.com/sua-org/cam-bus/internal/drivers"
	"github.com/sua-org/cam-bus/internal/engines"
	"github.com/sua-org/cam-bus/internal/mediamtx"
	"github.com/sua-org/cam-bus/internal/mqttclient"
	"github.com/sua-org/cam-bus/internal/uplink"
)

type Supervisor struct {
	mqtt      *mqttclient.Client
	baseTopic string

	shard   string
	engines *engines.Manager
	uplink  *uplink.Manager
	mtxGen  *mediamtx.Generator

	mu             sync.Mutex
	cameras        map[string]core.CameraInfo
	workers        map[string]*cameraWorker
	statusInterval time.Duration
	proc           *process.Process // <- NOVO: processo do cam-bus para métricas
}

type cameraWorker struct {
	info          core.CameraInfo
	cancel        context.CancelFunc
	lastEventAt   time.Time // última vez que vimos evento dessa câmera
	status        drivers.ConnectionState
	statusSince   time.Time
	statusReason  string
	everConnected bool
	analytics     []string
}

type workerSnapshot struct {
	Info          core.CameraInfo
	LastEventAt   time.Time
	Status        drivers.ConnectionState
	StatusSince   time.Time
	StatusReason  string
	EverConnected bool
	Analytics     []string
}

func (s *Supervisor) snapshotWorkers() []workerSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]workerSnapshot, 0, len(s.workers))
	for _, w := range s.workers {
		out = append(out, workerSnapshot{
			Info:          w.info,
			LastEventAt:   w.lastEventAt,
			Status:        w.status,
			StatusSince:   w.statusSince,
			StatusReason:  w.statusReason,
			EverConnected: w.everConnected,
			Analytics:     w.analytics,
		})
	}
	return out
}

// Atualiza última vez que recebemos evento dessa câmera
func (s *Supervisor) touchWorker(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if w, ok := s.workers[key]; ok {
		now := time.Now().UTC()
		w.lastEventAt = now
		if w.status != drivers.ConnectionStateOnline {
			w.status = drivers.ConnectionStateOnline
			w.statusSince = now
			w.statusReason = ""
		}
		w.everConnected = true
	}
}

func (s *Supervisor) updateWorkerStatus(key string, update drivers.StatusUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.workers[key]
	if !ok {
		return
	}

	now := time.Now().UTC()
	w.status = update.State
	w.statusReason = update.Reason
	w.statusSince = now
	if update.State == drivers.ConnectionStateOnline {
		w.everConnected = true
	}
}

func New(mqtt *mqttclient.Client, baseTopic string) *Supervisor {
	baseTopic = strings.TrimSuffix(baseTopic, "/")

	shard := os.Getenv("CAMBUS_SHARD")
	if shard == "" {
		log.Printf("[supervisor] CAMBUS_SHARD não definido (essa instância atende TODOS os shards)")
	} else {
		log.Printf("[supervisor] CAMBUS_SHARD=%s", shard)
	}

	eng := engines.LoadFromEnv()
	uplinkManager := uplink.NewManagerFromEnv()
	statusInterval := envDurationSeconds("CAMBUS_STATUS_INTERVAL_SECONDS", 30*time.Second)
	var procHandle *process.Process
	if p, err := process.NewProcess(int32(os.Getpid())); err == nil {
		procHandle = p
	}

	supervisor := &Supervisor{
		mqtt:           mqtt,
		baseTopic:      baseTopic,
		shard:          shard,
		engines:        eng,
		uplink:         uplinkManager,
		mtxGen:         mediamtx.NewGeneratorFromEnv(),
		cameras:        make(map[string]core.CameraInfo),
		workers:        make(map[string]*cameraWorker),
		statusInterval: statusInterval,
		proc:           procHandle,
	}
	if supervisor.uplink != nil {
		supervisor.uplink.SetStatusHook(supervisor.handleUplinkStatus)
	}
	return supervisor
}

func envDurationSeconds(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec <= 0 {
		log.Printf("[supervisor] valor inválido em %s=%q, usando default %s", key, v, def)
		return def
	}
	return time.Duration(sec) * time.Second
}

func hasAnalytic(info core.CameraInfo, name string) bool {
	for _, a := range info.Analytics {
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}

func (s *Supervisor) resolveActiveAnalytics(drv drivers.CameraDriver, info core.CameraInfo) []string {
	if reporter, ok := drv.(drivers.AnalyticsReporter); ok {
		if active := reporter.ActiveAnalytics(); len(active) > 0 {
			return active
		}
	}
	return info.Analytics
}

func slugForCamera(info core.CameraInfo) string {
	base := fmt.Sprintf("rtls_%s_%s_%s_%s",
		info.Tenant,
		info.Building,
		info.Floor,
		info.DeviceID,
	)
	base = strings.ToLower(base)
	base = strings.ReplaceAll(base, " ", "_")
	base = strings.ReplaceAll(base, "-", "_")
	return base
}

// publishHADiscovery publica entidades MQTT Discovery para o Home Assistant
// para uma câmera que tenha analítico faceRecognized.
func (s *Supervisor) publishHADiscovery(info core.CameraInfo) error {
	if s.engines == nil || !s.engines.Has("findface") {
		return nil
	}

	// A câmera precisa gerar eventos de face (faceCapture ou FaceDetection),
	// porque é isso que o faceengine consome para gerar faceRecognized.
	if !(hasAnalytic(info, "faceCapture") || hasAnalytic(info, "FaceDetection")) {
		return nil
	}

	slug := slugForCamera(info)
	deviceID := "rtls_camera_" + slug

	// tópico dos eventos de faceRecognized
	eventTopic := s.eventTopic(info, "faceRecognized")

	// objeto comum de device
	deviceObj := map[string]interface{}{
		"identifiers":  []string{deviceID},
		"name":         fmt.Sprintf("Câmera %s (%s %s, %s)", info.DeviceID, info.Building, info.Floor, info.Tenant),
		"manufacturer": info.Manufacturer,
		"model":        info.Model,
	}

	// 1) Binary sensor: alerta FaceRecognized
	binCfg := map[string]interface{}{
		"name":                  fmt.Sprintf("FaceRecognized %s", info.DeviceID),
		"unique_id":             slug + "_face_recognized",
		"state_topic":           eventTopic,
		"value_template":        "{% if value_json.AnalyticType == 'faceRecognized' and value_json.Meta.eventState == 'active' %}ON{% else %}OFF{% endif %}",
		"payload_on":            "ON",
		"payload_off":           "OFF",
		"expire_after":          10,
		"json_attributes_topic": eventTopic,
		"device":                deviceObj,
		"origin": map[string]interface{}{
			"name": "rtls-cam-bus",
		},
	}
	if err := s.publishDiscoveryConfig("binary_sensor", slug+"_face_recognized", binCfg); err != nil {
		return err
	}

	// 2) Sensor: CPF / ID pessoa
	personCfg := map[string]interface{}{
		"name":           fmt.Sprintf("Face Recognition CPF %s", info.DeviceID),
		"unique_id":      slug + "_face_person",
		"state_topic":    eventTopic,
		"value_template": "{{ value_json.Meta.ff_person_name }}",
		"icon":           "mdi:account",
		"device":         deviceObj,
		"origin": map[string]interface{}{
			"name": "rtls-cam-bus",
		},
	}
	if err := s.publishDiscoveryConfig("sensor", slug+"_face_person", personCfg); err != nil {
		return err
	}

	// 3) Sensor: mensagem amigável
	msgCfg := map[string]interface{}{
		"name":           fmt.Sprintf("Face Recognition Msg %s", info.DeviceID),
		"unique_id":      slug + "_face_message",
		"state_topic":    eventTopic,
		"value_template": "Reconhecido com a pessoa: {{ value_json.Meta.ff_person_name }}",
		"icon":           "mdi:account-badge",
		"device":         deviceObj,
		"origin": map[string]interface{}{
			"name": "rtls-cam-bus",
		},
	}
	if err := s.publishDiscoveryConfig("sensor", slug+"_face_message", msgCfg); err != nil {
		return err
	}

	// 4) Sensor: confiança
	confCfg := map[string]interface{}{
		"name":                fmt.Sprintf("Face Recognition Confiança %s", info.DeviceID),
		"unique_id":           slug + "_face_confidence",
		"state_topic":         eventTopic,
		"value_template":      "{{ (value_json.Meta.ff_confidence * 100) | round(1) }}",
		"unit_of_measurement": "%",
		"icon":                "mdi:shield-half-full",
		"device":              deviceObj,
		"origin": map[string]interface{}{
			"name": "rtls-cam-bus",
		},
	}
	if err := s.publishDiscoveryConfig("sensor", slug+"_face_confidence", confCfg); err != nil {
		return err
	}

	// 5) Sensor: horário
	timeCfg := map[string]interface{}{
		"name":           fmt.Sprintf("Face Recognition Horário %s", info.DeviceID),
		"unique_id":      slug + "_face_time",
		"state_topic":    eventTopic,
		"device_class":   "timestamp",
		"value_template": "{{ as_datetime(value_json.Timestamp) }}",
		"device":         deviceObj,
		"origin": map[string]interface{}{
			"name": "rtls-cam-bus",
		},
	}
	if err := s.publishDiscoveryConfig("sensor", slug+"_face_time", timeCfg); err != nil {
		return err
	}

	// 6) Entidade de imagem: snapshot (corrigindo localhost -> minio)
	imgCfg := map[string]interface{}{
		"name":         fmt.Sprintf("Face Snapshot %s", info.DeviceID),
		"unique_id":    slug + "_face_snapshot",
		"url_topic":    eventTopic,
		"url_template": "{{ value_json.SnapshotURL | replace('http://localhost:9000', 'http://minio:9000') }}",
		"device":       deviceObj,
		"origin": map[string]interface{}{
			"name": "rtls-cam-bus",
		},
	}
	if err := s.publishDiscoveryConfig("image", slug+"_face_snapshot", imgCfg); err != nil {
		return err
	}

	// 7) Entidade de imagem: foto da base (card do FindFace)
	dbImgCfg := map[string]interface{}{
		"name":         fmt.Sprintf("Face DB Photo %s", info.DeviceID),
		"unique_id":    slug + "_face_db_photo",
		"url_topic":    eventTopic,
		"url_template": "{{ value_json.Meta.ff_person_photo_url }}",
		"device":       deviceObj,
		"origin": map[string]interface{}{
			"name": "rtls-cam-bus",
		},
	}
	if err := s.publishDiscoveryConfig("image", slug+"_face_db_photo", dbImgCfg); err != nil {
		return err
	}

	return nil
}
func (s *Supervisor) runStatusLoop(ctx context.Context) {
	hostname, _ := os.Hostname()
	ticker := time.NewTicker(s.statusInterval)
	defer ticker.Stop()

	log.Printf("[supervisor] status loop iniciado (intervalo=%s)", s.statusInterval)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[supervisor] status loop encerrado (context canceled)")
			return
		case t := <-ticker.C:
			s.publishStatuses(hostname, t)
		}
	}
}

func (s *Supervisor) publishStatuses(hostname string, now time.Time) {
	workers := s.snapshotWorkers()
	if len(workers) == 0 {
		return
	}

	// NOVO: pegar métricas de CPU/Memória do processo cam-bus
	var (
		cpuPercent  float64
		memPercent  float64
		memRSSBytes uint64
	)

	if s.proc != nil {
		if cpu, err := s.proc.CPUPercent(); err == nil {
			cpuPercent = cpu
		}
		if memInfo, err := s.proc.MemoryInfo(); err == nil {
			memRSSBytes = memInfo.RSS
		}
		if memP, err := s.proc.MemoryPercent(); err == nil {
			memPercent = float64(memP)
		}
	}

	// Agrupamento por tenant + building (collector em nível de prédio)
	type buildingKey struct {
		Tenant   string
		Building string
	}

	buildingMap := make(map[buildingKey]int)

	// 1) Status das câmeras
	for _, w := range workers {
		bk := buildingKey{
			Tenant:   w.Info.Tenant,
			Building: w.Info.Building,
		}
		buildingMap[bk]++

		if err := s.publishCameraStatus(w, now); err != nil {
			log.Printf("[status] erro ao publicar status da câmera %s: %v", s.keyFor(w.Info), err)
		}
	}

	// 2) Status do collector por prédio
	for bk, camCount := range buildingMap {
		if err := s.publishCollectorStatusForBuilding(
			bk.Tenant,
			bk.Building,
			hostname,
			camCount,
			cpuPercent,
			memPercent,
			memRSSBytes,
			now,
		); err != nil {
			log.Printf(
				"[status] erro ao publicar status do collector para %s/%s: %v",
				bk.Tenant, bk.Building, err,
			)
		}
	}
}

func (s *Supervisor) publishCollectorStatusForBuilding(
	tenant, building, hostname string,
	cameras int,
	cpuPercent float64,
	memPercent float64,
	memRSSBytes uint64,
	now time.Time,
) error {
	payload := map[string]interface{}{
		"collector":        "cam-bus",
		"status":           "online",
		"timestamp":        now.UTC().Format(time.RFC3339),
		"hostname":         hostname,
		"shard":            s.shard,
		"cameras":          cameras,
		"cpu_percent":      cpuPercent,
		"memory_percent":   memPercent,
		"memory_rss_bytes": memRSSBytes,
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal collector status: %w", err)
	}

	topic := s.collectorStatusTopic(tenant, building)
	if err := s.mqtt.Publish(topic, 1, true, b); err != nil {
		return fmt.Errorf("publish collector status to %s: %w", topic, err)
	}

	log.Printf("[status] collector online -> %s", topic)
	return nil
}

func (s *Supervisor) publishCameraStatus(
	snap workerSnapshot,
	now time.Time,
) error {
	payload := map[string]interface{}{
		"tenant":      snap.Info.Tenant,
		"building":    snap.Info.Building,
		"floor":       snap.Info.Floor,
		"device_type": snap.Info.DeviceType,
		"device_id":   snap.Info.DeviceID,
		"status":      string(snap.Status),
		"timestamp":   now.UTC().Format(time.RFC3339),
	}

	if !snap.LastEventAt.IsZero() {
		payload["last_event_at"] = snap.LastEventAt.UTC().Format(time.RFC3339)
	}
	if snap.Info.Shard != "" {
		payload["shard"] = snap.Info.Shard
	}
	if !snap.StatusSince.IsZero() {
		payload["status_since"] = snap.StatusSince.UTC().Format(time.RFC3339)
	}
	if snap.StatusReason != "" {
		payload["status_reason"] = snap.StatusReason
	}
	if len(snap.Info.Analytics) > 0 {
		payload["analytics_configured"] = snap.Info.Analytics
	}
	if len(snap.Analytics) > 0 {
		payload["analytics_active"] = snap.Analytics
	}
	if snap.EverConnected {
		payload["ever_connected"] = snap.EverConnected
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal camera status: %w", err)
	}

	topic := s.cameraStatusTopic(snap.Info)
	if err := s.mqtt.Publish(topic, 1, true, b); err != nil {
		return fmt.Errorf("publish camera status to %s: %w", topic, err)
	}

	log.Printf("[status] camera status published -> %s", topic)
	return nil
}

func (s *Supervisor) publishDiscoveryConfig(component, objectID string, cfg map[string]interface{}) error {
	topic := fmt.Sprintf("homeassistant/%s/%s/config", component, objectID)
	payload, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal discovery %s: %w", topic, err)
	}

	// retain=true para o HA "lembrar" das entidades mesmo se cam-bus reiniciar
	if err := s.mqtt.Publish(topic, 1, true, payload); err != nil {
		return fmt.Errorf("publish discovery %s: %w", topic, err)
	}

	log.Printf("[supervisor] published HA discovery for %s: %s", component, topic)
	return nil
}

// Run assina os tópicos /info e gerencia as câmeras.
func (s *Supervisor) Run(ctx context.Context) error {
	infoTopic := fmt.Sprintf("%s/+/+/+/+/+/info", s.baseTopic) // rtls/cameras/tenant/building/floor/type/id/info
	log.Printf("[supervisor] subscribing to info topic: %s", infoTopic)
	uplinkTopic := fmt.Sprintf("%s/+/+/+/+/+/uplink/+", s.baseTopic)
	log.Printf("[supervisor] subscribing to uplink topic: %s", uplinkTopic)

	if err := s.mqtt.Subscribe(infoTopic, 1, s.handleInfoMessage); err != nil {
		return fmt.Errorf("subscribe error: %w", err)
	}
	if err := s.mqtt.Subscribe(uplinkTopic, 1, s.handleUplinkMessage); err != nil {
		return fmt.Errorf("subscribe uplink error: %w", err)
	}
	if s.statusInterval > 0 {
		go s.runStatusLoop(ctx)
	}

	<-ctx.Done()
	log.Printf("[supervisor] context canceled, stopping all workers")
	s.stopAll()
	return nil
}

func (s *Supervisor) handleInfoMessage(topic string, payload []byte) {
	// Esperado: base/tenant/building/floor/type/id/info
	// Exemplo de payload:
	// {
	//   "ip": "10.0.0.10",
	//   "name": "Portaria",
	//   "manufacturer": "Hikvision",
	//   "model": "DS-2CD",
	//   "username": "admin",
	//   "password": "secret",
	//   "port": 443,
	//   "use_tls": true,
	//   "enabled": true,
	//   "analytics": ["faceCapture"],
	//   "rtsp_url": "rtsp://10.0.0.10:554/Streaming/Channels/101",
	//   "proxy_path": "camera-001",
	//   "central_host": "central.local",
	//   "central_srt_port": 8890,
	//   "central_path": "hq/camera-001",
	//   "record_enabled": true,
	//   "record_retention_minutes": 1440,
	//   "pre_roll_seconds": 5
	// }
	parts := strings.Split(topic, "/")
	baseParts := strings.Split(s.baseTopic, "/")

	if len(parts) < len(baseParts)+6 {
		log.Printf("[supervisor] invalid info topic: %s", topic)
		return
	}

	offset := len(baseParts)
	tenant := parts[offset+0]
	building := parts[offset+1]
	floor := parts[offset+2]
	devType := parts[offset+3]
	devID := parts[offset+4]
	// parts[offset+5] == "info"

	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 || bytes.Equal(trimmedPayload, []byte("null")) {
		info := core.CameraInfo{
			Tenant:     tenant,
			Building:   building,
			Floor:      floor,
			DeviceType: devType,
			DeviceID:   devID,
		}
		key := s.keyFor(info)
		log.Printf("[supervisor] camera %s removed via tombstone", key)
		s.cleanupCamera(info)
		return
	}

	var info core.CameraInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		log.Printf("[supervisor] invalid JSON on %s: %v", topic, err)
		return
	}

	info.Tenant = tenant
	info.Building = building
	info.Floor = floor
	info.DeviceType = devType
	info.DeviceID = devID

	info.RTSPURL = strings.TrimSpace(info.RTSPURL)
	info.ProxyPath = strings.TrimSpace(info.ProxyPath)
	info.CentralHost = strings.TrimSpace(info.CentralHost)
	info.CentralPath = strings.TrimSpace(info.CentralPath)
	defaultProxyPath := strings.TrimSpace(info.DeviceID)
	if defaultProxyPath == "" {
		defaultProxyPath = fmt.Sprintf("%s_%s_%s_%s", info.Tenant, info.Building, info.Floor, info.DeviceID)
		defaultProxyPath = strings.Trim(defaultProxyPath, "_")
	}
	if info.ProxyPath == "" {
		info.ProxyPath = defaultProxyPath
	}
	if info.CentralPath == "" {
		info.CentralPath = uplink.CentralPathFor(info)
	}
	if info.RecordRetentionMinutes < 0 {
		log.Printf("[supervisor] record_retention_minutes inválido para %s, usando 0", info.DeviceID)
		info.RecordRetentionMinutes = 0
	}
	if info.RecordRetentionMinutes > 0 {
		info.RecordEnabled = true
	} else {
		info.RecordEnabled = false
	}
	if info.PreRollSeconds < 0 {
		log.Printf("[supervisor] pre_roll_seconds inválido para %s, usando 0", info.DeviceID)
		info.PreRollSeconds = 0
	}

	// TODO: filtro de shard, se quiser (shard por camera, etc.)

	key := s.keyFor(info)

	// Se a câmera estiver desabilitada, para worker
	if !info.Enabled {
		log.Printf("[supervisor] camera %s disabled via info topic, stopping worker", key)
		s.cleanupCamera(info)
		return
	}

	s.upsertCameraInfo(key, info)

	if s.uplink != nil {
		if strings.TrimSpace(info.CentralHost) != "" {
			req := uplink.Request{
				CameraID:       info.DeviceID,
				ProxyPath:      info.ProxyPath,
				CentralHost:    info.CentralHost,
				CentralSRTPort: info.CentralSRTPort,
				CentralPath:    info.CentralPath,
			}
			req.Normalize()
			if err := s.uplink.Start(req); err != nil {
				log.Printf("[uplink] start failed for %s: %v", req.CameraID, err)
			}
		} else {
			s.uplink.StopByCamera(info)
		}
	}

	// Publica discovery para o Home Assistant (se tiver faceRecognized)
	if err := s.publishHADiscovery(info); err != nil {
		log.Printf("[supervisor] erro ao publicar discovery para %s: %v", key, err)
	}

	// Por fim, inicia/atualiza o worker normalmente
	s.startOrUpdateCamera(info)
}

func (s *Supervisor) handleUplinkMessage(topic string, payload []byte) {
	parts := strings.Split(topic, "/")
	baseParts := strings.Split(s.baseTopic, "/")
	if len(parts) < len(baseParts)+7 {
		log.Printf("[uplink] invalid uplink topic: %s", topic)
		return
	}
	offset := len(baseParts)
	tenant := parts[offset+0]
	building := parts[offset+1]
	devID := parts[offset+4]
	action := parts[len(parts)-1]

	var req uplink.Request
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Printf("[uplink] invalid JSON on %s: %v", topic, err)
		return
	}
	req.Normalize()
	if strings.EqualFold(action, "start") && req.CentralPath == "" {
		if req.ProxyPath != "" {
			req.CentralPath = strings.Trim(req.ProxyPath, "/")
		} else {
			req.CentralPath = uplink.CentralPathFor(core.CameraInfo{
				Tenant:   tenant,
				Building: building,
				DeviceID: devID,
			})
		}
	}
	if err := req.Validate(); err != nil {
		log.Printf("[uplink] invalid payload on %s: %v", topic, err)
		return
	}

	switch strings.ToLower(action) {
	case "start":
		if err := s.uplink.Start(req); err != nil {
			log.Printf("[uplink] start failed for %s: %v", req.CameraID, err)
		}
	case "stop":
		if err := s.uplink.Stop(req); err != nil {
			log.Printf("[uplink] stop failed for %s: %v", req.CameraID, err)
		}
	default:
		log.Printf("[uplink] unknown uplink action: %s", action)
	}
}

func (s *Supervisor) handleUplinkStatus(status uplink.Status) {
	info, ok := s.findCameraInfoForUplinkStatus(status)
	if !ok {
		log.Printf("[uplink] status without camera info (cameraId=%s centralPath=%s container=%s state=%s)",
			status.CameraID, status.CentralPath, status.ContainerName, status.State)
		return
	}
	topic := s.uplinkStatusTopic(info)
	payload, err := json.Marshal(status)
	if err != nil {
		log.Printf("[uplink] status marshal failed for %s: %v", topic, err)
		return
	}
	if err := s.mqtt.Publish(topic, 1, false, payload); err != nil {
		log.Printf("[uplink] status publish failed for %s: %v", topic, err)
	}
}

func (s *Supervisor) findCameraInfoForUplinkStatus(status uplink.Status) (core.CameraInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizedCentral := strings.Trim(strings.TrimSpace(status.CentralPath), "/")
	for _, info := range s.cameras {
		if status.CameraID != "" && strings.EqualFold(info.DeviceID, status.CameraID) {
			return info, true
		}
		if normalizedCentral != "" && strings.Trim(strings.TrimSpace(info.CentralPath), "/") == normalizedCentral {
			return info, true
		}
	}
	return core.CameraInfo{}, false
}

func (s *Supervisor) uplinkStatusTopic(info core.CameraInfo) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s/%s/uplink/status",
		s.baseTopic,
		info.Tenant,
		info.Building,
		info.Floor,
		info.DeviceType,
		info.DeviceID,
	)
}

func (s *Supervisor) keyFor(info core.CameraInfo) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s",
		info.Tenant, info.Building, info.Floor, info.DeviceType, info.DeviceID)
}

// cameraInfoEqual compara se duas configs de câmera são equivalentes
// para o propósito de decidir se precisamos reiniciar o driver.
func cameraInfoEqual(a, b core.CameraInfo) bool {
	if a.Tenant != b.Tenant ||
		a.Building != b.Building ||
		a.Floor != b.Floor ||
		a.DeviceType != b.DeviceType ||
		a.DeviceID != b.DeviceID ||
		a.Name != b.Name ||
		a.Manufacturer != b.Manufacturer ||
		a.Model != b.Model ||
		a.IP != b.IP ||
		a.Port != b.Port ||
		a.Username != b.Username ||
		a.Password != b.Password ||
		a.UseTLS != b.UseTLS ||
		a.Enabled != b.Enabled ||
		a.Shard != b.Shard ||
		a.RTSPURL != b.RTSPURL ||
		a.ProxyPath != b.ProxyPath ||
		a.CentralPath != b.CentralPath ||
		a.RecordEnabled != b.RecordEnabled ||
		a.RecordRetentionMinutes != b.RecordRetentionMinutes ||
		a.PreRollSeconds != b.PreRollSeconds {
		return false
	}

	// compara analytics (se existir)
	if len(a.Analytics) != len(b.Analytics) {
		return false
	}
	for i := range a.Analytics {
		if a.Analytics[i] != b.Analytics[i] {
			return false
		}
	}

	return true
}

func (s *Supervisor) startOrUpdateCamera(info core.CameraInfo) {
	key := s.keyFor(info)

	s.mu.Lock()
	shouldRefresh := false
	defer func() {
		s.mu.Unlock()
		if shouldRefresh {
			go s.refreshMediaMTXConfig()
		}
	}()

	if w, ok := s.workers[key]; ok {
		// Já existe worker para essa câmera.
		if cameraInfoEqual(w.info, info) {
			log.Printf("[supervisor] camera %s already running with same config, ignoring update", key)
			return
		}

		// Config mudou => reinicia worker.
		log.Printf("[supervisor] camera %s config changed, restarting worker", key)
		w.cancel()
		delete(s.workers, key)
		shouldRefresh = true
	}

	drv, err := drivers.GetDriver(info)
	if err != nil {
		log.Printf("[supervisor] no driver for camera %s: %v", key, err)
		go s.refreshMediaMTXConfig()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	eventsCh := make(chan core.AnalyticEvent, 64)
	analytics := s.resolveActiveAnalytics(drv, info)

	worker := &cameraWorker{
		info:         info,
		cancel:       cancel,
		status:       drivers.ConnectionStateConnecting,
		statusSince:  time.Now().UTC(),
		statusReason: "aguardando conexão",
		analytics:    analytics,
	}

	s.workers[key] = worker
	shouldRefresh = true

	if statusAware, ok := drv.(drivers.StatusAwareDriver); ok {
		statusAware.SetStatusHandler(func(update drivers.StatusUpdate) {
			s.updateWorkerStatus(key, update)
		})
	}

	log.Printf("[supervisor] starting camera worker %s (%s %s, shard=%s)", key, info.Manufacturer, info.Model, info.Shard)

	// Goroutine que roda o driver (Hikvision, etc.)
	go func() {
		defer func() {
			cancel()
			close(eventsCh)
		}()
		if err := drv.Run(ctx, eventsCh); err != nil {
			log.Printf("[worker %s] driver ended with error: %v", key, err)
		} else {
			log.Printf("[worker %s] driver ended gracefully", key)
		}
	}()

	// Goroutine que publica eventos no MQTT e aciona engines (pós-processadores)
	go func() {
		defer s.updateWorkerStatus(key, drivers.StatusUpdate{State: drivers.ConnectionStateOffline, Reason: "event stream encerrado"})
		for evt := range eventsCh {
			// 1) publica evento original (faceCapture, FaceDetection, PeopleCounting, etc.)
			s.touchWorker(key)
			// Faz uma cópia só para publicação, sem o base64 (para não explodir o MQTT).
			evtOut := evt
			evtOut.SnapshotB64 = ""

			topic := s.eventTopic(info, evtOut.AnalyticType)
			payload, err := json.Marshal(evtOut)
			if err != nil {
				log.Printf("[worker %s] error marshaling event: %v", key, err)
			} else {
				if err := s.mqtt.Publish(topic, 1, false, payload); err != nil {
					log.Printf("[worker %s] error publishing to %s: %v", key, topic, err)
				} else {
					log.Printf("[worker %s] published event to %s (event_id=%s)", key, topic, evt.EventID)
				}
			}

			// 2) Engines: geram eventos derivados (ex.: faceRecognized)
			if s.engines != nil && s.engines.Enabled() {
				derived, _ := s.engines.ProcessAll(ctx, evt)
				for _, dEvt := range derived {
					outEvt := dEvt
					outEvt.SnapshotB64 = ""

					outTopic := s.eventTopic(info, outEvt.AnalyticType)
					outPayload, err := json.Marshal(outEvt)
					if err != nil {
						log.Printf("[worker %s] erro ao marshalar evento derivado (%s): %v", key, outEvt.AnalyticType, err)
						continue
					}
					if err := s.mqtt.Publish(outTopic, 1, false, outPayload); err != nil {
						log.Printf("[worker %s] erro ao publicar evento derivado (%s) em %s: %v", key, outEvt.AnalyticType, outTopic, err)
						continue
					}
					log.Printf("[worker %s] published derived event (%s) -> %s (event_id=%s)", key, outEvt.AnalyticType, outTopic, outEvt.EventID)
				}
			}
		}
	}()
}

func (s *Supervisor) eventTopic(info core.CameraInfo, analyticType string) string {
	analyticType = strings.TrimSpace(analyticType)
	if analyticType == "" {
		analyticType = "unknown"
	}
	return fmt.Sprintf("%s/%s/%s/%s/%s/%s/%s/events",
		s.baseTopic,
		info.Tenant,
		info.Building,
		info.Floor,
		info.DeviceType,
		info.DeviceID,
		analyticType,
	)
}

func (s *Supervisor) cameraStatusTopic(info core.CameraInfo) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s/%s/status",
		s.baseTopic,
		info.Tenant,
		info.Building,
		info.Floor,
		info.DeviceType,
		info.DeviceID,
	)
}
func (s *Supervisor) collectorStatusTopic(tenant, building string) string {
	return fmt.Sprintf("%s/%s/%s/collector/status",
		s.baseTopic,
		tenant,
		building,
	)
}

func (s *Supervisor) stopCamera(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.workers[key]
	if !ok {
		return
	}

	log.Printf("[supervisor] stopping camera worker %s", key)
	w.cancel()
	delete(s.workers, key)
}

func (s *Supervisor) stopAll() {
	s.mu.Lock()
	infosByKey := make(map[string]core.CameraInfo, len(s.cameras)+len(s.workers))
	for key, info := range s.cameras {
		infosByKey[key] = info
	}
	for key, w := range s.workers {
		infosByKey[key] = w.info
	}
	s.mu.Unlock()

	for _, info := range infosByKey {
		s.cleanupCamera(info)
	}
}

func (s *Supervisor) cleanupCamera(info core.CameraInfo) {
	key := s.keyFor(info)
	log.Printf("[supervisor] cleanup camera %s (handleInfoMessage/stopAll)", key)
	s.stopCamera(key)
	s.removeCameraInfo(key)
	if s.uplink != nil {
		s.uplink.StopByCamera(info)
	}
	s.refreshMediaMTXConfig()
}

func (s *Supervisor) refreshMediaMTXConfig() {
	if s.mtxGen == nil {
		return
	}

	infos := s.snapshotCameraInfos()
	if err := s.mtxGen.Sync(infos); err != nil {
		log.Printf("[supervisor] erro ao atualizar config do MediaMTX: %v", err)
	}
}

func (s *Supervisor) snapshotCameraInfos() []core.CameraInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	infos := make([]core.CameraInfo, 0, len(s.cameras))
	for _, info := range s.cameras {
		infos = append(infos, info)
	}
	return infos
}

func (s *Supervisor) upsertCameraInfo(key string, info core.CameraInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cameras[key] = info
}

func (s *Supervisor) removeCameraInfo(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cameras, key)
}
