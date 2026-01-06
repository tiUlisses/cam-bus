## Uplink START payload

Regra de naming para `centralPath`: `tenant/building/deviceId`.

Exemplo de payload (START):

```json
{
  "cameraId": "camera-001",
  "proxyPath": "camera-001",
  "centralHost": "central.mediamtx.local",
  "centralSrtPort": 8890,
  "centralPath": "acme/hq/camera-001",
  "ttlSeconds": 30
}
```

## MediaMTX Central com paths dinâmicos

Para aceitar publicações dinâmicas, a configuração do MediaMTX central deve ter
`pathDefaults` com `source: publisher` (ex.: `infra/mediamtx/central/mediamtx.yml`).
