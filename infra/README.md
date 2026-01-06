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

## Reload HTTP do MediaMTX

Para recarregar o MediaMTX via HTTP, configure a URL e as credenciais (se houver):

```bash
MTX_PROXY_RELOAD_URL="http://mediamtx.local:9997/v3/reload"
MTX_PROXY_RELOAD_USER="admin"
MTX_PROXY_RELOAD_PASS="secret"
```

Ou usando token bearer:

```bash
MTX_PROXY_RELOAD_URL="http://mediamtx.local:9997/v3/reload"
MTX_PROXY_RELOAD_TOKEN="seu-token"
```
