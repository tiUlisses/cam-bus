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

Para recarregar o MediaMTX via HTTP, configure a URL e as credenciais (se houver).
Se o MediaMTX estiver com API autenticada, um `401` indica credenciais ausentes
ou inválidas.

### Habilitar autenticação da API no MediaMTX

No `mediamtx.yml` do proxy (ex.: `infra/mediamtx/proxy/mediamtx.yml`), habilite
as credenciais conforme a versão do MediaMTX:

```yml
api: yes
apiAddress: :9997

# Usuário/senha (versões recentes)
apiUser: admin
apiPass: secret

# OU token bearer (dependendo da versão)
# apiToken: seu-token
```

### Configurar o .env do cam-bus

Use usuário/senha:

```bash
MTX_PROXY_RELOAD_URL="http://mediamtx.local:9997/v3/reload"
MTX_PROXY_RELOAD_USER="admin"
MTX_PROXY_RELOAD_PASS="secret"
```

Se o cam-bus reescrever o `mediamtx.yml` do proxy, defina também as credenciais
da API para preservá-las no arquivo gerado:

```bash
MTX_PROXY_API_USER="admin"
MTX_PROXY_API_PASS="secret"
```

Ou usando token bearer:

```bash
MTX_PROXY_RELOAD_URL="http://mediamtx.local:9997/v3/reload"
MTX_PROXY_RELOAD_TOKEN="seu-token"
```

Com token bearer, use:

```bash
MTX_PROXY_API_TOKEN="seu-token"
```
