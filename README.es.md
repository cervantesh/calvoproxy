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

## Arranque rápido

Tres comandos desde cero hasta un proxy funcionando:

```bash
calvoproxy login     # login por navegador con OpenRouter; guarda una key por usuario
calvoproxy           # arranca en http://127.0.0.1:8080
calvoproxy doctor    # verifica toda la cadena y el cableado de tu cliente
```

`doctor` es el atajo que te salta todas las trampas de abajo. Comprueba, en el
orden en que realmente las chocás: que el proxy responda, que las credenciales
resuelvan, que **una petición real sobreviva la cadena completa** y —si tenés
Hermes instalado— que Hermes esté cableado para pasar por el proxy. Cada fallo
imprime el arreglo, y si algo falla imprime el bloque de config exacto para
pegar. Con `--no-live` se salta la petición real.

### Cablear Hermes (leé esto antes de tocar config.yaml)

Hacen falta dos claves, y si falta cualquiera de las dos falla **en silencio**:
Hermes sigue funcionando, solo que habla con OpenRouter (o con tu proveedor
anterior) en vez de con el proxy.

```yaml
model:
  provider: custom
  default: coding
  base_url: http://127.0.0.1:8080/v1   # OBLIGATORIO — ver abajo
  api_key: dummy                       # la key real vive en el proxy

custom_providers:
  - name: calvoproxy
    base_url: http://127.0.0.1:8080/v1  # debe coincidir EXACTO con model.base_url
    api_key: dummy
    api_mode: chat_completions
    discover_models: false              # el proxy no sirve /v1/models
    models:
      coding:    {context_length: 131072}
      simple:    {context_length: 131072}
      reasoning: {context_length: 131072}
      vision:    {context_length: 131072}
```

- **`model.base_url` es lo que enlaza `provider: custom` con la entrada de
  `custom_providers`.** Sin eso Hermes resuelve `provider: custom` pero se
  queda con `base_url: https://openrouter.ai/api/v1`, manda el nombre de perfil
  `coding` al upstream y recibe `400: coding is not a valid model ID`.
- **Usá `127.0.0.1`, no `localhost`.** Hermes solo confía en `model.base_url`
  si el host es loopback, y `localhost` puede resolver a IPv6 `::1` mientras el
  proxy escucha solo en IPv4.
- **El gateway NO relee `config.yaml` en caliente.** Reinicialo después de
  editar, o no cambia nada.

Confirmá que surtió efecto — los contadores del propio proxy son la única
prueba:

```bash
curl -s http://127.0.0.1:8080/metrics | grep calvoproxy_requests_by_status
```

Mandá un mensaje por Hermes y mirá que suba `class="2xx"`. Si no se mueve, la
petición nunca llegó al proxy.

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
| `CEREBRAS_API_KEY`   | —       | Key directa para el fallback de Cerebras |
| `GROQ_API_KEY`       | —       | Key directa para el fallback de Groq |
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
| `PROXY_SCORING_RECOVERY_SECONDS` | `21600` (6 h) | Mitad de wall-clock de la ventana de decay del score: cuánto tarda en perdonarse del todo un modelo degradado |
| `PROXY_SCORING_RECOVERY_ATTEMPTS` | `50` | Mitad de evidencia de la misma ventana: cuántos intentos scoreados más, a nivel proxy, hacen falta. El decay avanza al ritmo del **más lento** de los dos, así un proxy ocioso no olvida |
| `PROXY_SCORE_FILE` | `<dir-de-config>/calvoproxy/scores.json` | Dónde se persisten los scores aprendidos entre reinicios. Poné `off` para desactivar la persistencia |
| `PROXY_SCORE_MAX_AGE_SECONDS` | `86400` (24 h) | Descarta un archivo de scores (y entradas individuales) más viejo que esto |
| `PROXY_BREAKER_FAILURE_THRESHOLD` | `3` | Fallos consecutivos antes de abrir el circuito de un modelo |
| `PROXY_BREAKER_COOLDOWN_SECONDS` | `60` | Cuánto tiempo un circuito abierto saltea un modelo |
| `PROXY_OPENROUTER_URL` | OpenRouter | Override del endpoint de chat de OpenRouter (ej. un mock) |
| `PROXY_AGENTIC_URL`  | off     | Si se setea, los perfiles `agent`/`plan` van acá; sin setear → ruteo normal a OpenRouter |
| `PROXY_WORKSPACE_SIDE_EFFECTS` | `false` | Extractor git/sqlite del monorepo, opt-in (apagado por defecto) |
| `PROXY_ADMIN_TOKEN`  | off     | Si se setea, protege `/health`, `/metrics`, `/health/model-policy`, `/admin/reload` tras un token Bearer (comparación constant-time) |
| `PROXY_VAULT_FILE` | `<dir-de-config>/calvoproxy/providers.vault` | Ruta del vault cifrado de proveedores |
| `PROXY_VAULT_MASTER_KEY_FILE` | off | Archivo explícito de 32 bytes para Linux sin systemd; rechaza symlinks y permisos de grupo/mundo |
| `PROXY_ADMIN_ALLOW_INSECURE_REMOTE` | `false` | Permite la consola de keys por HTTP remoto sin TLS; no recomendado |
| `PROXY_METRICS_TOKEN` | off    | Si se setea, `/metrics` acepta este token O el admin — desacopla la credencial del scraper de la de admin |
| `PROXY_ALLOW_ENV_KEY_PUBLIC` | `false` | Permite gastar la `OPENROUTER_API_KEY` del entorno para requests sin key en un bind **público** (loopback siempre lo permite) |
| `PROXY_OAUTH_REQUIRE_STATE` | `true` | Exige un `state` CSRF coincidente en el callback de `calvoproxy login`. OpenRouter lo devuelve, así que viene activado; poné `false` solo para un proveedor que no lo haga (siguen aplicando el path secreto + PKCE) |
| `PROXY_UPDATE_CHECK` | `true`  | Chequeo al arranque de una versión más nueva (loguea una recomendación). Poné `false` para desactivar |

Las métricas Prometheus están en **`/metrics`** (score por-modelo, fallos
consecutivos, éxitos, cantidad de circuitos abiertos, readiness, más tasa de
requests, conteos por clase de status, suma/conteo de latencia, desenlaces de
stream (`completed`/`stalled`/`upstream_error`/`max_duration`/`client_gone`),
rechazos por admisión, rechazos por capacidad, conteo de requests gRPC y un
gauge `build_info`). Cuando `PROXY_ADMIN_TOKEN` está seteado, los endpoints detallados
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
  removido, y se recupera hacia una línea de base neutral para reintentarse
  después. Los scores se ven en `circuits[].score` en `/health` y como
  `calvoproxy_model_score` en `/metrics`. Poné `PROXY_SCORING_ENABLED=false`
  para mantener el orden estático de la cadena.

**Cómo se recupera un score.** El decay se mide contra dos relojes y avanza al
ritmo del **más lento**: el wall-clock transcurrido
(`PROXY_SCORING_RECOVERY_SECONDS`, default 6 h) y la cuenta de intentos
scoreados a nivel proxy (`PROXY_SCORING_RECOVERY_ATTEMPTS`, default 50). El
reloj de intentos es el que importa: nada cambia en un modelo mientras nadie lo
llama, así que un hueco de inactividad no debe borrar lo aprendido. Solo los
intentos posteriores —evidencia nueva real, a favor o en contra— mueven un score
de vuelta hacia su línea de base.

Antes de v0.9.2 el decay era puro wall-clock sobre una ventana de cinco minutos,
lo que dejaba todo el subsistema inerte en una carga interactiva y a ráfagas:
cada score convergía a la línea neutral durante cualquier hueco entre sesiones,
la cadena quedaba re-rankeada exactamente en su orden configurado, y la ráfaga
siguiente volvía a pagar entero el costo de descubrimiento. En una instancia real
eso se vio como 29% de los requests abandonados en el presupuesto de primer
evento, y como un modelo con 96 éxitos scoreando idéntico (0.8000) a uno con 0
éxitos y un fallo vigente.

**Modelos que nunca tuvieron éxito.** Un modelo que no acertó ni una vez en la
memoria del proxy deriva hacia una línea de base más baja (`0.5`) que uno que
solo tuvo un mal día (`0.8`). Cero éxitos no es la misma evidencia que un mal
día: un mal día está contradicho por los éxitos que lo rodean. Ese modelo igual
se intenta —último— y un solo éxito real lo pasa a la línea normal para siempre.

**Persistencia.** Los scores se escriben en `PROXY_SCORE_FILE` (default
`<dir-de-config>/calvoproxy/scores.json`) cada 30 s cuando cambian, y una vez más
en un apagado limpio; se recargan al arrancar. Antes de v0.9.2 el mapa de scores
vivía solo en memoria, así que cada reinicio —incluido instalar un build nuevo—
descartaba todo lo aprendido. Límites deliberados:

- **El estado del breaker no se persiste.** Un cooldown es una afirmación sobre
  el ahora, y un reinicio es una buena razón para volver a sondear. Solo
  sobreviven el score, sus dos lecturas de reloj y la cuenta de éxitos.
- **Los archivos viejos se descartan** enteros, igual que las entradas
  individuales más viejas que `PROXY_SCORE_MAX_AGE_SECONDS` (default 24 h). Los
  slugs del tier gratuito se retiran y re-aprovisionan en esa escala de tiempo.
- **Las claves que la policy actual ya no nombra se descartan** al cargar. Editar
  una cadena es justamente la señal de que cambió el set de modelos, y una clave
  que no está en ninguna cadena solo se alcanza con un pin explícito, donde la
  cadena tiene un solo modelo y el score no ordena nada.
- Poné `PROXY_SCORE_FILE=off` para desactivar la persistencia del todo. Nunca se
  escribe nada en el directorio de trabajo: si no se puede determinar un
  directorio de config, la persistencia simplemente queda apagada.
- **En un contenedor, montá un volumen.** La imagen fija
  `PROXY_SCORE_FILE=/data/scores.json` y crea `/data` escribible por cualquier
  UID, así también funciona con `--user`; sin un volumen montado ahí, los scores
  se pierden en cada recreación. Compose ya declara uno.

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
  El slot se retiene durante **todo** el request, streams incluidos (esa es la
  idea: un stream vivo sigue ocupando una conexión upstream), así que subí el
  tope en deploys con muchos streams.
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

### Elegir un perfil

Los clientes piden un **nombre de perfil**, no un id de modelo. El proxy lo
resuelve a una cadena ordenada y baja por ella ante fallo o rate-limit.

| Perfil | Para | Al agotarse la cadena |
|---|---|---|
| `coding` (por defecto) | código, y trabajo de agente con tools | degrada, cola débil permitida |
| `reasoning` | análisis, planificación, diseño | degrada, pero nunca por debajo de ~12B |
| `critic` | revisión adversarial, juicios de corrección | **503 — nunca degrada** |
| `bulk` | resúmenes, clasificación, borradores | degrada libremente |
| `vision` | peticiones con imágenes | solo modelos con visión |

Alias: `simple`→`bulk`, `agent`/`creative`→`coding`, `review`/`adversarial`→`critic`,
`planning`/`design`→`reasoning`.

**Los perfiles se distinguen por el fallo que tolerás, no por el nombre de la
tarea.** `coding` y `bulk` pueden caer a un modelo chico porque una respuesta vale
más que ninguna. `critic` no: para una revisión, una respuesta confiada y errónea es
peor que ninguna, así que su cadena no tiene cola débil y devuelve **503** cuando
todos sus miembros están caídos. Reintentá, o escalá a un revisor más fuerte.

Un nombre de perfil que no existe ahora da **404**, no una sustitución silenciosa —
un typo, o un cliente escrito contra documentación anterior a la política, no debe
recibir respuesta de la cadena que la clasificación por palabras clave eligió.

**Cómo leer los nombres.** El tamaño está en el slug, y lo que importa es la
cantidad de parámetros **activos** en un modelo mixture-of-experts:
`nemotron-3-ultra-550b-a55b` son 550B totales pero **55B activos por token**;
`nemotron-3-nano-30b-a3b` son **3B activos**. Eso es una diferencia de 18× en
cómputo real entre dos modelos que ambos anuncian "reasoning". `nano`, `mini`,
`xs` y `flash` significan pequeño u optimizado para latencia.

**Las cadenas degradan en silencio, y eso es una virtud con filo.** Si el
primer modelo está rate-limiteado responde el segundo, y así. Para trabajo en
bloque, una respuesta vale más que ninguna. Para una tarea donde una respuesta
*equivocada* es peor que ninguna —una revisión adversarial, un juicio de
corrección— una cadena que cae a un modelo mucho más chico te da una respuesta
confiada en la que no deberías confiar. Nada en el status HTTP te avisa.

Dos defensas, ambas disponibles hoy:

```bash
# La respuesta nombra el modelo que realmente la sirvió.
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer dummy" \
  -d '{"model":"reasoning","messages":[{"role":"user","content":"hola"}]}' \
  | jq -r '.model'
```

- **Revisá las cabeceras de la respuesta.** Toda respuesta nombra el modelo que
  la sirvió, así que el llamador nunca tiene que adivinar:

  | Cabecera | Significado |
  |---|---|
  | `X-Calvoproxy-Model` | el modelo que respondió |
  | `X-Calvoproxy-Profile` | el perfil bajo el que se ruteó |
  | `X-Calvoproxy-Attempt` | posición en la cadena — **más de 1 fue un fallback** |

  `X-Calvoproxy-Attempt` es la señal de degradación que un status HTTP nunca te
  da. El campo `model` del cuerpo lleva el mismo id para quien parsee el cuerpo.
- **Revisá `.model` en todo lo que pienses creerle.** Un nombre de perfil es un
  pedido, no una garantía.
- **Mantené los modelos débiles fuera de las cadenas que usás para juzgar.** El
  suelo de calidad se impone por *omisión*: el scoring de fiabilidad puede
  reordenar una cadena pero nunca puede meter un modelo que no esté listado en
  ella, así que una cadena cuyos miembros están todos por encima de tu barra se
  mantiene por encima.

`/v1/embeddings` se rechaza por defecto (**402**): OpenRouter no publica ningún
modelo de embeddings gratuito, así que ese endpoint gasta crédito real y es el único
camino sin cadena, breaker ni fallback detrás. Habilitalo con
`PROXY_ALLOW_PAID_EMBEDDINGS=true`.

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

  **La verificación de firma está ACTIVA en los builds oficiales.** Además del
  checksum SHA-256, el actualizador verifica una **firma Ed25519** sobre
  `SHA256SUMS.txt`, que autentica una release contra un host/cuenta comprometidos
  (algo que un checksum solo no puede). La clave pública viaja en
  [`internal/releasekey`](internal/releasekey/key.go), así que `calvoproxy update`
  es **fail-closed en la firma**: una `SHA256SUMS.txt.sig` ausente o inválida
  rechaza la actualización, y `--insecure` no puede saltearla.

  Por eso el workflow de release **falla ruidosamente** si falta el secret
  `RELEASE_SIGNING_KEY` mientras hay una clave embebida (publicar sin firmar
  rompería la actualización de todos los clientes), y verifica la firma recién
  generada contra esa misma clave embebida antes de publicar.

  Para un **fork**, configurá tu propia firma una sola vez:

  1. Generá un par de claves: `go run ./tools/gen`.
  2. Poné la clave **pública** impresa en
     [`internal/releasekey/key.go`](internal/releasekey/key.go) (seguro de
     commitear) — o pasala por `PROXY_UPDATE_PUBKEY` en runtime.
  3. Agregá la clave **privada** impresa como el secret de GitHub Actions
     `RELEASE_SIGNING_KEY` (repo → Settings → Secrets → Actions).

  Dejar la clave vacía desactiva la verificación de firma (solo SHA-256); el
  workflow entonces permite releases sin firmar sin fallar.

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
por el mismo motor de ruteo. Es **solo unary**: un request con `stream: true` se **rechaza** con
`InvalidArgument` en vez de bufferear el stream entero en memoria; usá la API
HTTP para streaming de tokens. El proto vive en
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

## Consola de keys de proveedores

Definí un `PROXY_ADMIN_TOKEN` fuerte, iniciá CalvoProxy y abrí
`http://127.0.0.1:8080/admin/providers`. La consola web administra una key
cifrada para OpenRouter, Cerebras y Groq, y permite probarla sin devolvérsela al
navegador. La interfaz y todos sus mensajes operacionales están en inglés.

El formato del vault es común, pero la llave maestra aleatoria de 256 bits queda
bajo el sistema operativo: DPAPI CurrentUser en Windows, Keychain en builds
nativos de macOS, y una credencial systemd o archivo protegido explícito en
Linux headless. Ver [`docs/linux-headless-vault.md`](docs/linux-headless-vault.md).
No existe fallback implícito a texto plano; si falta la llave maestra el vault
queda bloqueado y las variables de entorno siguen funcionando.

Precedencia efectiva:

- OpenRouter: `Authorization` del request → entorno → vault → archivo login legado.
- Cerebras: entorno → vault.
- Groq: entorno → vault.

Las credenciales ambientales de cualquier origen se rechazan para tráfico sin
key sobre un bind público salvo que `PROXY_ALLOW_ENV_KEY_PUBLIC=true`.

## Iniciar sesión en OpenRouter (`calvoproxy login`)

En vez de copiar y pegar una API key de la dashboard, autorizá CalvoProxy vía el
OAuth (PKCE) de OpenRouter — cómodo para onboarding, ya que cada usuario trae su
propia key revocable:

```bash
calvoproxy login          # abre tu navegador → autorizás → key guardada localmente
calvoproxy whoami         # muestra qué key hay configurada (enmascarada) y su origen
calvoproxy logout         # borra la key guardada
```

`login` levanta un servidor de callback loopback de un solo uso en un **path
inadivinable** (32 bytes aleatorios), abre `https://openrouter.ai/auth`, y tras
autorizar intercambia el code por una API key **controlada por el usuario**
(verificado vía PKCE `S256`).

El path secreto es lo que ata el callback a *tu* login: otro proceso en la misma
máquina puede encontrar el puerto pero no el path, así que no puede ni inyectar su
propio code de autorización ni matarte el login con basura —y, a diferencia del
parámetro `state` de OAuth, esa protección no depende de que el proveedor lo
devuelva—. (No protege contra un atacante del mismo usuario que pueda leer la URL
de autorización desde el historial del navegador o la línea de comandos; el
secreto viaja en esa URL, igual que viajaría `state`.) Un `state` que no coincide
y un `error=` no atribuible se ignoran en vez de terminar el login.

Además, un `state` CSRF coincidente es **obligatorio por defecto**
(`PROXY_OAUTH_REQUIRE_STATE`) — un login interactivo confirmó que OpenRouter lo
devuelve, así que exigirlo cierra el agujero de login-CSRF por completo en vez de
apoyarse solo en el path secreto. Poné `false` únicamente si tu proveedor no
devuelve `state`; el login sigue funcionando, protegido por el path secreto y PKCE. La key se escribe en
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
  -v calvoproxy-scores:/data \
  ghcr.io/cervantesh/calvoproxy:latest
```

> **Por qué el volumen.** El proxy aprende cuáles de sus modelos gratis
> responden de verdad y reordena la cadena en consecuencia. Eso se escribe en
> `/data/scores.json`; sin volumen se pierde cada vez que se recrea el
> contenedor, y la cadena vuelve a pagar entero el costo de descubrimiento en la
> ráfaga siguiente. Compose ya declara un volumen nombrado. Omitilo solo si
> realmente querés un contenedor sin estado.

> **Exponerlo de forma segura.** El contenedor bindea `0.0.0.0`, así que es
> alcanzable por cualquier cosa que llegue al puerto. Por eso **no** gasta su
> `OPENROUTER_API_KEY` con clientes que no mandan su propia key —esos reciben un
> `401`—. Dos perillas deciden qué tan abierto está:
>
> - `-e PROXY_ADMIN_TOKEN=…` — protege `/health`, `/metrics` y `/admin/reload`
>   (si no, quedan legibles por cualquiera).
> - `-e PROXY_ALLOW_ENV_KEY_PUBLIC=true` — permite que clientes sin key gasten la
>   key del contenedor. Hacelo solo si al puerto llega **únicamente** gente a la
>   que le darías la key.
>
> Los clientes que mandan su propio `Authorization: Bearer sk-or-…` siempre
> funcionan y no se ven afectados.

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
- `test/contract/` — tests opt-in contra la API **real** de OpenRouter. Los mocks
  codifican supuestos; estos verifican los supuestos. Se corren con
  `CALVOPROXY_CONTRACT=1 OPENROUTER_API_KEY=... go test ./test/contract/ -v`.
- `vendor/` — dependencias vendorizadas (no editar a mano).

## Cambios y dependencias

- [CHANGELOG.md](CHANGELOG.md) — cada release, qué cambió y de qué falla vino.
  Las retractaciones quedan en el registro.
- [THIRD_PARTY.md](THIRD_PARTY.md) — los módulos `cervo-*` **no tienen entradas
  en `go.sum` ni un upstream alcanzable**. Leé esto antes de confiar en el build,
  auditarlo o forkearlo.

Además de la suite de tests, CI aplica tres trinquetes: un piso de cobertura
(`scripts/coverage-gate.sh`), un manifiesto de checksums sobre los módulos
vendorizados (`scripts/vendor-manifest.sh`) y — cuando el secret del repositorio
está presente — los tests de contrato contra el upstream.
