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

## Modos de uplink (container vs mediamtx)

Por padrão (`UPLINK_MODE=container`), o cam-bus inicia um container FFmpeg para
republish RTSP->SRT ao central. Esse modo depende do Docker disponível no host.

No modo `UPLINK_MODE=mediamtx`, o republish é feito pelo MediaMTX proxy usando
`runOnReady` por path. O cam-bus deixa de iniciar containers e passa a gerar o
`mediamtx.yml` do proxy com:

- `record: yes` herdado de `pathDefaults` (ex.: `infra/mediamtx/proxy/mediamtx.yml`);
- `sourceOnDemand: yes` para só republish quando a fonte estiver pronta;
- `runOnReady` chamando FFmpeg com `-c copy` e `streamid=publish:<centralPath>`.

Para habilitar:

```bash
UPLINK_MODE=mediamtx
MTX_PROXY_CONFIG_PATH=/caminho/para/mediamtx.yml
```

Se preferir manter o modo atual, use:

```bash
UPLINK_MODE=container
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

# Usuário/senha (MediaMTX recente)
authInternalUsers:
  - user: any
    permissions:
      - action: publish
      - action: read
      - action: playback
  - user: admin
    pass: secret
    permissions:
      - action: api
```

### Configurar o .env do cam-bus

Use usuário/senha. Se o cam-bus estiver no mesmo `docker-compose`/rede do
`infra/mediamtx/docker-compose.yml`, prefira apontar para o serviço do proxy:

```bash
MTX_PROXY_RELOAD_URL="http://mtx-proxy:9997"
```

Se quiser manter o hostname atual, crie um `network_alias: mediamtx.local` no
serviço `mtx-proxy` do compose e continue usando `mediamtx.local`.

Use usuário/senha:

```bash
MTX_PROXY_RELOAD_URL="http://mediamtx.local:9997"
MTX_PROXY_RELOAD_USER="admin"
MTX_PROXY_RELOAD_PASS="secret"
```

Se o cam-bus reescrever o `mediamtx.yml` do proxy, defina também as credenciais
da API para gerar `authInternalUsers` no arquivo:

```bash
MTX_PROXY_API_USER="admin"
MTX_PROXY_API_PASS="secret"
```

Ou usando token bearer apenas para o reload:

```bash
MTX_PROXY_RELOAD_URL="http://mediamtx.local:9997"
MTX_PROXY_RELOAD_TOKEN="seu-token"
```
