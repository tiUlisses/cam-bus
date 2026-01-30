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

## Uplink always-on

Para iniciar uplinks automaticamente na inicialização do supervisor, configure:

```bash
UPLINK_ALWAYS_ON=true
```

Para restringir o always-on a algumas câmeras, defina uma lista de paths (separados por vírgula, espaço ou ponto e vírgula):

```bash
UPLINK_ALWAYS_ON_PATHS="acme/hq/camera-001,camera-002"
```

A lista aceita `centralPath`, `proxyPath` ou `cameraId` e é comparada sem `/` nas bordas e sem diferenciar maiúsculas/minúsculas.
Quando o always-on está ativo, o supervisor aciona `uplink.Start` no carregamento das câmeras, o TTL informado é ignorado para evitar encerramento automático e comandos de stop são ignorados.

## IGNORE_UPLINK

Quando `IGNORE_UPLINK=yes`, o cam-bus ignora comandos de start/stop e TTLs, tratando todas as câmeras como always-on.
Nesse modo, o supervisor garante `centralHost` e `centralPath` para todas as câmeras:

- `centralHost` usa `UPLINK_CENTRAL_HOST` (quando presente).
- `centralPath` é gerado a partir de `uplink.CentralPathFor(info)` quando vazio.

Use este modo quando quiser que o uplink sempre permaneça ativo e o MediaMTX replique todas as câmeras automaticamente.

## Modos de uplink (container vs mediamtx)

Por padrão (`UPLINK_MODE=container`), o cam-bus inicia um container FFmpeg para
republish RTSP->SRT ao central. Esse modo depende do Docker disponível no host.

No modo `UPLINK_MODE=mediamtx`, o republish é feito pelo MediaMTX proxy usando
`runOnReady` por path. O cam-bus deixa de iniciar containers e passa a gerar o
`mediamtx.yml` do proxy com:

- `record: yes` herdado de `pathDefaults` (ex.: `infra/mediamtx/proxy/mediamtx.yml`);
- `sourceOnDemand: no` para o proxy conectar na origem e disparar `runOnReady`;
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
