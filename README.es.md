# CalvoProxy

*[English](README.md) · Español*

Proxy inteligente compatible con OpenAI que pone modelos gratuitos de OpenRouter
detrás de un solo endpoint. Aplica una política de request determinista, elige
una cadena de modelos por request, y agrega funciones de gateway —timeouts,
reintentos, circuit breaking, scoring de fiabilidad, límites y auditoría— encima
del reenvío al upstream.

CalvoProxy es **totalmente autocontenido**: todas sus dependencias están
**vendorizadas** (`vendor/`), así que compila y corre offline, sin acceso a
módulos externos.

## Compilar

```bash
go build -mod=vendor -o calvoproxy ./cmd
```

La compilación es totalmente offline —cada dependencia vive bajo `vendor/`—. Lo
podés comprobar con `GOPROXY=off go build ./cmd`.

> Como las dependencias están vendorizadas (algunas vienen de un registro privado
> no resoluble públicamente), la compilación siempre usa `vendor/`. **No** corras
> `go mod tidy` / `go get` acá —intentaría re-resolver esos módulos privados por
> red—. Para cambiar dependencias, trabajá en el monorepo fuente y re-vendorizá.

## Ejecutar

```bash
./calvoproxy
```

El servidor expone una API HTTP compatible con OpenAI. El streaming
(`stream: true`) se reenvía con flushing; `SIGINT`/`SIGTERM` drenan los requests
en vuelo antes de salir.

| Variable de entorno  | Default | Descripción                          |
|----------------------|---------|--------------------------------------|
| `HOST`               | `127.0.0.1` | Dirección de bind. Loopback por defecto (mantiene el proxy y su env key fuera de la red). Poné `0.0.0.0` para exponerlo — la imagen Docker lo hace automáticamente |
| `PORT`               | `8080`  | Puerto HTTP                          |
| `GRPC_PORT`          | `9090`  | Puerto gRPC (ver [gRPC](#transporte-grpc)); un fallo de bind no es fatal |
| `OPENROUTER_API_KEY` | —       | Key del upstream para el ejecutor por defecto |
| `PROXY_IDLE_TIMEOUT` | off     | Sale tras este período de inactividad (duración Go, ej. `20m`) — habilita el uso on-demand |
| `PROXY_MAX_BODY_BYTES` | `10485760` | Body de request máximo (10 MiB) — protege contra payloads gigantes |
| `PROXY_MAX_RESPONSE_BYTES` | `26214400` | Respuesta upstream no-stream máxima en memoria (25 MiB) — protege contra OOM |
| `PROXY_REQUEST_TIMEOUT_SECONDS` | `45` | Timeout por-intento (una llamada upstream). La llegada de headers en streams también se acota con esto |
| `PROXY_TOTAL_TIMEOUT_SECONDS` | `120` | Presupuesto total de wall-clock a lo largo de la cadena de fallback (no-stream) |
| `PROXY_STREAM_IDLE_TIMEOUT` | `120` | Máximo hueco (segundos) entre chunks streameados antes de abortar un stream estancado |
| `PROXY_STREAM_MAX_DURATION` | `1800` | Tope absoluto (segundos) para la vida de un solo stream; `0` desactiva el backstop |
| `PROXY_MAX_IDLE_CONNS_PER_HOST` | `128` | Tamaño del pool de conexiones idle por host upstream. Subirlo por encima del default de 2 de la stdlib evita el churn de conexiones bajo concurrencia |
| `PROXY_MAX_CONCURRENT` | off | Tope de requests concurrentes en vuelo. Una ráfaga que supera el tope espera y luego recibe `503 Retry-After`; evita que un pico estampide el upstream más allá de sus rate limits |
| `PROXY_ADMISSION_TIMEOUT_SECONDS` | `5` | Cuánto espera un request por encima del tope antes del `503` (solo cuando `PROXY_MAX_CONCURRENT` está seteado) |
| `PROXY_SCORING_ENABLED` | `true` | Reordena la cadena por score de fiabilidad por-modelo (ver abajo) |
| `PROXY_BREAKER_FAILURE_THRESHOLD` | `3` | Fallos consecutivos antes de abrir el circuito de un modelo |
| `PROXY_BREAKER_COOLDOWN_SECONDS` | `60` | Cuánto tiempo un circuito abierto saltea un modelo |
| `PROXY_OPENROUTER_URL` | OpenRouter | Override del endpoint de chat de OpenRouter (ej. un mock) |
| `PROXY_AGENTIC_URL`  | off     | Si se setea, los perfiles `agent`/`plan` van acá; sin setear → ruteo normal a OpenRouter |
| `PROXY_WORKSPACE_SIDE_EFFECTS` | `false` | Extractor git/sqlite del monorepo, opt-in (apagado por defecto) |
| `PROXY_ADMIN_TOKEN`  | off     | Si se setea, protege `/health`, `/metrics`, `/health/model-policy`, `/admin/reload` tras un token Bearer (comparación constant-time) |
| `PROXY_METRICS_TOKEN` | off    | Si se setea, `/metrics` acepta este token O el admin — desacopla la credencial del scraper de la de admin |
| `PROXY_ALLOW_ENV_KEY_PUBLIC` | `false` | Permite gastar la `OPENROUTER_API_KEY` del entorno para requests sin key en un bind **público** (loopback siempre lo permite) |
| `PROXY_UPDATE_CHECK` | `true`  | Chequeo al arranque de una versión más nueva (loguea una recomendación). Poné `false` para desactivar |

Las métricas Prometheus están en **`/metrics`** (score por-modelo, fallos
consecutivos, éxitos, cantidad de circuitos abiertos, readiness, más tasa de
requests, conteos por clase de status, suma/conteo de latencia y un gauge
`build_info`). Cuando `PROXY_ADMIN_TOKEN` está seteado, los endpoints detallados
lo requieren; `/ready` queda abierto y solo devuelve readiness.

> **Defaults seguros.** `HOST` es loopback (`127.0.0.1`) por defecto, y los
> endpoints admin/metrics/health quedan abiertos solo en ese bind loopback. Si
> exponés el proxy (`HOST=0.0.0.0`, o vía Docker), **seteá `PROXY_ADMIN_TOKEN`** —
> si no, esos endpoints quedan legibles por cualquiera. En un bind público el
> proxy además rehúsa gastar la `OPENROUTER_API_KEY` del entorno para requests sin
> key salvo que `PROXY_ALLOW_ENV_KEY_PUBLIC=true`, así una instancia expuesta no
> se vuelve un relay abierto a tu costa. Un warning al arranque avisa si lo
> exponés sin token.

**Hot-reload** de las cadenas de modelos sin reiniciar: editá `model-policy.json`
y después `kill -HUP <pid>` (Unix) o `POST /admin/reload` (cualquier plataforma,
protegido por admin).

### Ruteo por capacidad (visión + tools)

No todos los modelos gratuitos aceptan imágenes o hacen tool calling. CalvoProxy
etiqueta cada modelo con sus capacidades y, cuando un request necesita una,
filtra la cadena a los modelos que realmente la soportan antes de
breaker/scoring/fallback — así una foto va a un modelo con visión y un request
con tool calling va a un modelo capaz de tools (y un request que necesita
**ambas** va a un modelo que hace las dos).

- **Detección:** contenido de imagen ⇒ necesita `vision`; un array `tools`/
  `functions` ⇒ necesita `tools`. Los requests de texto plano nunca se filtran
  (cero cambio).
- **Fail-closed:** un modelo sin data de capacidad conocida **no** califica, así
  que imágenes/tools nunca se rutean silenciosamente a un modelo incapaz. Si fijás
  un `model` específico que no puede hacer lo que el request necesita, obtenés un
  `422` claro.
- **Rescue:** si el perfil elegido no tiene modelo capaz, el router recurre a
  cualquier modelo capaz conocido dentro de los perfiles; si no existe ninguno, un
  `503` claro.
- **Fuente de capacidades (híbrida):** auto-derivada de la API pública `/models`
  de OpenRouter (`input_modalities`/`supported_parameters`), **combinada con**
  overrides manuales en `model-policy.json` (autoritativos — usá `"!vision"`/
  `"!tools"` para denegar una capacidad mal reportada):

  ```json
  "Capabilities": {
    "google/gemma-4-31b-it:free": ["vision", "tools"],
    "openai/gpt-oss-20b:free": ["tools"]
  }
  ```

  El auto-derivado corre en background (`PROXY_CAPABILITY_AUTODERIVE=false` para
  desactivar; `PROXY_CAPABILITY_REFRESH_SECONDS` para cambiar el intervalo de 6h);
  los overrides curados cubren los modelos de la cadena de forma síncrona, así que
  también funciona offline.

### Fiabilidad: circuit breaker + scoring

Dos capas mantienen a los modelos inestables fuera del camino:

- **Circuit breaker** (compuerta dura): tras `PROXY_BREAKER_FAILURE_THRESHOLD`
  fallos consecutivos el circuito de un modelo **se abre** y se saltea por
  completo durante `PROXY_BREAKER_COOLDOWN_SECONDS`; un éxito lo cierra.
- **Score de fiabilidad** (ranking blando): cada modelo lleva un score en `[0,1]`
  que sube con éxitos y baja con fallos —más fuerte para rate-limits (429),
  errores de servidor (5xx) y timeouts, y también baja por 404 de "modelo no
  disponible"—. La cadena elegible se **reordena por score** (más fiable primero)
  antes de cada request, así un modelo con problemas se hunde al fondo sin ser
  removido, y **se recupera hacia neutral en ~5 min** para reintentarse después.
  Los scores se ven en `circuits[].score` en `/health`. Poné
  `PROXY_SCORING_ENABLED=false` para mantener el orden estático de la cadena.

### Capacidad y tuning

CalvoProxy en sí no es el cuello de botella —una prueba de carga (200 workers, 25k
requests contra un upstream rápido) sostuvo **~7.500 req/s con p99 ~156 ms y cero
errores de transporte**, y `/health` se mantuvo responsivo—. El límite práctico
para cargas reales es el rate limit del **upstream**: el tier gratuito de
OpenRouter rate-limitea mucho antes de que el proxy transpire, así que una ráfaga
de ~30 requests concurrentes gratuitos degrada mayormente a un `503` limpio (vía
la cadena de fallback, honrando el `Retry-After` del upstream). Para uso de baja
concurrencia estilo Hermes/Claude-Code tenés muchísimo margen.

Tuning bajo carga:

- **`PROXY_MAX_IDLE_CONNS_PER_HOST`** (default 128) — la perilla más importante en
  alta concurrencia; muy baja = churn de conexiones (agotamiento de puertos/
  threads), muy alta = sockets desperdiciados.
- **`PROXY_MAX_CONCURRENT`** — seteala para suavizar ráfagas: en vez de estampidar
  el upstream (y colapsar la cadena a 503), los requests de más esperan hasta
  `PROXY_ADMISSION_TIMEOUT_SECONDS` y luego reciben `503 Retry-After`.
- Las perillas de breaker/timeout (`PROXY_BREAKER_*`,
  `PROXY_REQUEST_TIMEOUT_SECONDS`, `PROXY_TOTAL_TIMEOUT_SECONDS`) modelan qué tan
  agresivamente se descarta un upstream inestable.

**Alertas** — los contadores de `/metrics` mapean a las alertas SLO típicas:
alertá por una suba sostenida de `calvoproxy_requests_by_status{class="5xx"}`
relativa a `calvoproxy_requests_total` (cadena agotada / upstream caído), por
`calvoproxy_open_circuits > 0` persistente (modelos trabados abiertos), y por la
latencia media derivada (`calvoproxy_request_latency_seconds_sum / _count`)
cruzando tu presupuesto. `calvoproxy_build_info{version=...}` etiqueta el build.

Reproducí o extendé estas mediciones con el harness en
[`test/load/`](test/load/); una versión reducida corre en CI como guardia de
regresión.

### Operación on-demand

Seteá `PROXY_IDLE_TIMEOUT` (ej. `20m`) y el proxy sale solo cuando no llega ningún
request de proxy dentro de la ventana (las sondas de health/readiness no cuentan).
Combinalo con un launcher que arranque el proxy solo cuando se necesita por
primera vez —ej. un hook de inicio de sesión que corre un pequeño script "arrancá
si el puerto está cerrado"—. El proxy entonces corre solo mientras se usa. Sin
setear, corre hasta que lo maten (siempre encendido).

Overrides adicionales de política/comportamiento (`PROXY_DEFAULT_PROVIDER`,
`PROXY_PROVIDER_FALLBACKS_JSON`, `PROXY_LIMITS_JSON`, `PROXY_RETRY_POLICY_JSON`,
…) están documentados en [docs/POLICY.md](docs/POLICY.md).

### Cadenas de modelos (editar sin recompilar)

Las cadenas de modelos por perfil viven en **`model-policy.json`** —la fuente viva
y editable—. Cambialo y reiniciá; no hace falta recompilar. Los slugs gratuitos de
OpenRouter se retiran periódicamente, así que este es el archivo que tocás cuando
un modelo empieza a dar 404 (y la cadena de fallback ya avanza sola al siguiente).

Orden de carga (el último gana):

1. Default embebido (`internal/router/config/model-policy.default.json`) — una
   base de último recurso para que el binario siempre arranque con una política
   válida.
2. `model-policy.json` — buscado en `PROXY_MODEL_POLICY_FILE`, luego junto al
   ejecutable, luego en el directorio de trabajo.
3. Overrides por env (`PROXY_PROVIDER_PROFILES_JSON`, `PROXY_DEFAULT_PROFILE`, …)
   para cambios puntuales/efímeros.

Refrescá la lista de modelos gratuitos desde OpenRouter:

```bash
curl -s https://openrouter.ai/api/v1/models -H "Authorization: Bearer $OPENROUTER_API_KEY" \
  | jq -r '.data[] | select(.id|endswith(":free")) | select((.pricing.prompt|tonumber)==0) | .id'
```

### Actualizaciones (self-update + aviso)

CalvoProxy conoce su propia versión y chequea GitHub Releases por una más nueva.

- **Al arranque** (solo builds versionados) hace un chequeo best-effort no
  bloqueante y, si existe una release más nueva, loguea una recomendación —`correr
  calvoproxy update` en una instalación de binario, o una línea `docker pull`
  cuando detecta que corre en un contenedor—. Desactivá con `PROXY_UPDATE_CHECK=false`.
- **`GET /version`** reporta el build corriendo y el resultado cacheado del
  chequeo: `{"version":"v0.2.2","latest":"v0.2.3","update_available":true,"checked":true}`.
- **`calvoproxy update`** (instalaciones de binario) actualiza en el lugar:
  descarga el archivo de release para tu OS/arch, **verifica su SHA-256** contra el
  `SHA256SUMS.txt` de la release, extrae el binario y lo intercambia atómicamente
  (en Windows el exe viejo se mueve a `calvoproxy.exe.old` y se limpia al próximo
  arranque). Reiniciá después para correr la versión nueva. La verificación es
  **fail-closed**: si una release no tiene `SHA256SUMS.txt` (o no hay entrada que
  matchee) la actualización se rechaza —pasá `--insecure` para forzar (inseguro;
  solo saltea el checksum, no HTTPS)—. `--force` reinstala aunque ya esté al día.
  `calvoproxy version` solo imprime la versión.

  ```bash
  calvoproxy update
  ```

  **Verificación de firma (opcional, recomendada).** Además del checksum SHA-256,
  el actualizador puede verificar una **firma Ed25519** sobre `SHA256SUMS.txt`, que
  autentica una release contra un host/cuenta comprometidos (algo que un checksum
  solo no puede). Está **apagada hasta que la actives** —setup de una sola vez:

  1. Generá un par de claves: `go run ./tools/gen`.
  2. Pegá la clave **pública** impresa en `releasePublicKey` en
     [`cmd/verify.go`](cmd/verify.go) (seguro de commitear) — o pasala por
     `PROXY_UPDATE_PUBKEY` en runtime.
  3. Agregá la clave **privada** impresa como el secret de GitHub Actions
     `RELEASE_SIGNING_KEY` (repo → Settings → Secrets → Actions).

  El workflow de release entonces firma `SHA256SUMS.txt` → `SHA256SUMS.txt.sig` en
  cada tag. Una vez configurada una clave pública, `calvoproxy update` es
  **fail-closed también en la firma**: una firma ausente o inválida rechaza la
  actualización. Hasta que setees una clave, el comportamiento no cambia (solo
  SHA-256).

  Dentro de un contenedor el self-update se rechaza a propósito (una imagen es
  inmutable) — bajá un tag nuevo y recreá:

  ```bash
  docker pull ghcr.io/cervantesh/calvoproxy:latest
  docker compose up -d   # o: docker rm -f calvoproxy && docker run … :latest
  ```

### Fiabilidad de streams largos

Las respuestas streameadas (`stream: true`) **no** están acotadas por el timeout
por-request —una completion larga pero viva se entrega completa—. En cambio, un
stream se corta solo si se *estanca*: sin bytes por `PROXY_STREAM_IDLE_TIMEOUT`
(default 120s), con un backstop absoluto `PROXY_STREAM_MAX_DURATION` (default
30m). Los requests no-stream reciben un timeout por-intento más un presupuesto
total de wall-clock a lo largo de la cadena, así que un primer modelo lento no
puede matar de hambre a los fallbacks.

### Transporte gRPC

Junto a la API HTTP, CalvoProxy expone un pequeño `ProxyTransportService` gRPC
(unary `ChatCompletion` + `GetHealth`) en `GRPC_PORT` (default `9090`), respaldado
por el mismo motor de ruteo. Es **unary/buffered** —no un RPC de streaming—, así
que usá la API HTTP para streaming de tokens. El proto vive en
`proto/calvoproxy/proxy/v1/transport.proto`; los stubs generados están bajo
`gen/proto/proxyv1/`. Cuando `PROXY_ADMIN_TOKEN` está seteado, `GetHealth` lo
requiere vía metadata gRPC (`authorization: Bearer <token>`); `ChatCompletion`
siempre necesita una API key. Un fallo de bind en `GRPC_PORT` no es fatal —el
proxy HTTP sigue sirviendo—. (Compose mapea solo `8080`; publicá `9090` vos mismo
si necesitás gRPC.)

### Endpoints HTTP

- `GET /health` — estado del servicio, hashes de política activa, perfiles configurados.
- `GET /version` — build corriendo + si hay una release más nueva disponible.
- `POST /v1/chat/completions` — chat completions compatible con OpenAI.
- Rutas por perfil: `/v1/{simple,coding,reasoning,agent,creative,vision}/chat/completions`.
- `POST /v1/messages` — mensajes compatibles con Anthropic, ruteados por la misma
  cadena de modelos / breaker / scoring / fallback multi-modelo que chat (apunta a
  la forma `/messages` de OpenRouter/Anthropic; otros providers no la exponen).
- `POST /v1/embeddings` — embeddings.

Chequeo rápido:

```bash
curl -s http://127.0.0.1:8080/health
```

## Iniciar sesión en OpenRouter (`calvoproxy login`)

En vez de copiar y pegar una API key de la dashboard, autorizá CalvoProxy vía el
OAuth (PKCE) de OpenRouter — cómodo para onboarding, ya que cada usuario trae su
propia key revocable:

```bash
calvoproxy login          # abre tu navegador → autorizás → key guardada localmente
calvoproxy whoami         # muestra qué key hay configurada (enmascarada) y su origen
calvoproxy logout         # borra la key guardada
```

`login` levanta un servidor de callback loopback de un solo uso, abre
`https://openrouter.ai/auth`, y tras autorizar intercambia el code por una API key
**controlada por el usuario** (verificado vía PKCE `S256`). La key se escribe en
`<dir-config-usuario>/calvoproxy/openrouter.key` (`%AppData%` en Windows,
`~/.config` en Linux, `~/Library/Application Support` en macOS), con `0600`.

- `--no-browser` imprime la URL para abrir a mano.
- `--key-stdin` guarda una key pasada por pipe, sin navegador (`echo sk-or-v1-… | calvoproxy login --key-stdin`) — para headless/CI.

**Precedencia de la key** para un request sin key: header `Authorization` del
request → env `OPENROUTER_API_KEY` → la key de login guardada. La key guardada es
**ambient** como la del env, así que en un bind público se rehúsa salvo que
`PROXY_ALLOW_ENV_KEY_PUBLIC=true` (una key en el header siempre gana y saltea la
compuerta). Para deploys públicos/Docker, inyectá `OPENROUTER_API_KEY` o pasá un
Bearer por-request en vez de depender del archivo de login.

## Instalar

### Docker (recomendado)

Bajá la imagen publicada y correla con tu key de OpenRouter:

```bash
docker run -d --name calvoproxy -p 8080:8080 \
  -e OPENROUTER_API_KEY=sk-or-v1-... \
  ghcr.io/cervantesh/calvoproxy:latest
```

O con Compose (seteá `OPENROUTER_API_KEY` en tu shell o un archivo `.env`):

```bash
docker compose up -d
```

Para editar las cadenas de modelos gratuitos sin recompilar, montá tu propio archivo:

```bash
docker run -d -p 8080:8080 -e OPENROUTER_API_KEY=sk-or-v1-... \
  -v "$PWD/model-policy.json:/app/model-policy.json:ro" \
  ghcr.io/cervantesh/calvoproxy:latest
```

Construí la imagen localmente en vez de bajarla: `docker build -t calvoproxy .`

**¿Puerto ya en uso?** Si `8080` está tomado en el host (un `kube-apiserver`
local, otro servicio, …), mapeá un puerto de host distinto —el proxy sigue
escuchando en `8080` dentro del contenedor:

```bash
docker run -d -p 18080:8080 -e OPENROUTER_API_KEY=sk-or-v1-... \
  ghcr.io/cervantesh/calvoproxy:latest
# luego usá http://localhost:18080
```

**¿`docker pull` dice `denied` aunque la imagen es pública?** Tu Docker tiene
credenciales `ghcr.io` viejas. Limpialas y bajala anónimamente:

```bash
docker logout ghcr.io
docker pull ghcr.io/cervantesh/calvoproxy:latest
```

### Binarios precompilados (Windows / macOS / Linux)

Bajá el archivo para tu plataforma desde la página de
[Releases](https://github.com/cervantesh/calvoproxy/releases). Cada archivo
(`calvoproxy-<version>-<os>-<arch>.zip`/`.tar.gz`) contiene:

- `calvoproxy` (o `calvoproxy.exe` en Windows) — el servidor, un único binario estático;
- `model-policy.json` — las cadenas de modelos gratuitos editables (opcional; el
  binario tiene un default embebido, así que corre sin este archivo);
- `README.md`, `LICENSE`.

**Windows** (desde la carpeta extraída):

```powershell
$env:OPENROUTER_API_KEY = "sk-or-v1-..."
.\calvoproxy.exe
```

**macOS / Linux:**

```bash
export OPENROUTER_API_KEY=sk-or-v1-...
./calvoproxy
```

En macOS el binario no está firmado, así que la primera corrida puede necesitar
`xattr -d com.apple.quarantine ./calvoproxy` (o aprobarlo en System Settings →
Privacy & Security). El proxy entonces escucha en `http://localhost:8080` —apuntá
cualquier cliente compatible con OpenAI a él—. Conseguí una key gratuita de
OpenRouter en <https://openrouter.ai/keys>.

## Plugin de Claude Code

Este repo también trae un plugin de Claude Code para que Claude Code (no solo
Hermes) pueda consultar los modelos gratuitos a través del proxy —para segundas
opiniones, subtareas baratas y borradores—. Está bajo
[`plugins/calvoproxy/`](plugins/calvoproxy/) y este repo funciona además como
marketplace de plugins:

```text
/plugin marketplace add cervantesh/calvoproxy
/plugin install calvoproxy@calvoproxy
```

Después usá `/ask-free <prompt>` o pedilo naturalmente ("segunda opinión del
modelo gratis"). Config mínima: un proxy alcanzable (o seteá `CALVOPROXY_BIN` +
`OPENROUTER_API_KEY` para que el plugin lo arranque on-demand). Ver el
[README del plugin](plugins/calvoproxy/README.md).

## Estructura

- `cmd/` — entrypoint del servidor (wiring HTTP, idle shutdown, métricas, login, update).
- `internal/router/` — clasificación de requests, evaluación de política, intentos
  de modelo, reintentos, circuit breaker, ruteo por capacidad.
- `internal/telemetry/` — setup de OpenTelemetry.
- `docs/POLICY.md` — integración de CervoRules v3 / CervoModelPolicy y overrides de runtime.
- `vendor/` — dependencias vendorizadas (no editar a mano).
