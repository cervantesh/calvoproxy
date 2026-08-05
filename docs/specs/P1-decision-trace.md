# P1 — Traza de decisión de routing

Estado: **borrador, pendiente de aprobación**. Fase A del método SDD; ningún código de
producción hasta que esta spec esté aprobada.

Arquitectura de referencia: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md).

## 1. Problema

`setServedModelHeaders` ([router_http.go:76](../../internal/router/router_http.go)) ya dice
**qué** modelo respondió (`X-Calvoproxy-Model`, `-Profile`, `-Attempt`). No dice **por qué**:
qué modelos se descartaron y por qué motivo, con qué score se ordenó la cadena, qué intentos
fallaron antes y con qué código. Hoy eso solo vive en un `slog.InfoContext` en
`dispatchChain` ([router.go:311](../../internal/router/router.go)) y se pierde para el cliente.

## 2. Esquema de la traza

Estructura interna, un puntero por request, en `internal/router/router_trace.go`.

```go
type routeTrace struct {
    ID        string    // 16 hex chars, crypto/rand
    StartedAt time.Time

    Profile        string   // determineProfile ya resuelto por alias
    RequestedModel string   // "" si el cliente no pinó modelo
    RuleID         string   // policyDecision.RuleID
    CapsRequired   []string // nil cuando no se pidió vision/tools

    Planned   int // len tras planModelAttempts
    AfterCaps int // len tras applyCapabilityFilter
    Eligible  int // len tras filterAvailableAttempts + rank + truncado

    Excluded []traceExclusion
    Attempts []traceAttempt

    Served      *traceAttempt        // nil si ningún intento tuvo éxito
    Compression *traceCompression    // nil hasta P3; el header dice cmp=off
    Outcome     string               // served | all_cooling | caps_none | caps_pinned | chain_failed
}

type traceExclusion struct {
    Model string
    Why   string     // breaker | capability
    Until time.Time  // solo breaker; cero en el resto
}

type traceAttempt struct {
    Model  string
    Index  int       // 1-based, igual que modelAttempt.AttemptIndex
    Score  float64   // score en el momento de ordenar la cadena
    Status int       // attemptError.StatusCode; 200 en el servido
    Kind   string    // ok | http | transport | skip | unavailable | stream_abort | probe_busy
    Reason string    // SOLO canal admin — ver §6
    Millis int64
}
```

`Kind` es un enumerado cerrado. Es el campo que viaja por los canales públicos; `Reason` es
texto libre de origen upstream y no lo hace.

## 3. Formato del header corto

Cabecera `X-Calvoproxy-Route`, **valor único**, ASCII, `;` como separador de campos y `,`
entre entradas de una lista. El primer campo es siempre la versión.

```
X-Calvoproxy-Route: v1;p=coding;s=0.83;a=2;n=4/4/3;caps=tools;prev=gpt-oss-20b:429,gemma-4-31b:skip;brk=1;cmp=off
```

| Campo | Presencia | Significado |
|---|---|---|
| `v1` | siempre, primero | versión del formato |
| `p=` | siempre | perfil resuelto |
| `s=` | si hay servido | score del modelo servido al ordenar, 2 decimales |
| `a=` | si `AttemptIndex > 1` | posición en la cadena (señal de degradación) |
| `n=` | siempre | `Planned/AfterCaps/Eligible` |
| `caps=` | si se requirieron | `tools`, `vision` o `tools+vision` |
| `prev=` | si hubo fallos | `modelo:código` por intento fallido, en orden |
| `brk=` | si hubo exclusiones por breaker | número de modelos excluidos |
| `cmp=` | **siempre** | `off` hasta P3; después `-3.1k` o similar |
| `o=` | si `Outcome != served` | el valor de `Outcome` |
| `trunc=1` | si se recortó | ver §3.2 |

**Nombres de modelo abreviados** en el header: se elimina el prefijo de organización y el
sufijo `:free`. `nvidia/nemotron-3-super-120b-a12b:free` → `nemotron-3-super-120b-a12b`. El
nombre completo ya viaja en `X-Calvoproxy-Model` y en el JSON.

### 3.1 Códigos en `prev=`

Un entero HTTP, o uno de estos literales cuando no hay estatus HTTP significativo: `skip`
(`SkipModel`), `unavail` (`isModelUnavailable`), `probe` (probe half-open ocupado), `trans`
(error de transporte), `stream` (stream abortado).

### 3.2 Tope duro de 512 bytes

Si el valor supera 512 bytes se recorta de forma **determinista**, en este orden, hasta caber:

1. eliminar entradas de `prev=` desde el final, una a una;
2. si sigue sin caber, eliminar `prev=` entero;
3. si sigue sin caber, eliminar `brk=` y `n=`.

Tras cualquier recorte se añade `;trunc=1`. Los campos `v1`, `p=`, `cmp=` y `o=` no se
eliminan nunca: si con solo esos se superasen 512 bytes sería un bug, y el test del
invariante 4 lo cubre con el peor caso construible.

## 4. Puntos de anotación

| Dónde | Qué escribe |
|---|---|
| `dispatchChain` ([router.go:263](../../internal/router/router.go)) | crea la traza y la mete en el `ctx`; `Profile`, `RequestedModel`, `RuleID`, `CapsRequired` |
| tras `planModelAttempts` (:286) | `Planned` |
| `applyCapabilityFilter` (:372) | `AfterCaps` + una `traceExclusion{Why:"capability"}` por descarte |
| tras `filterAvailableAttempts` (:303) | `traceExclusion{Why:"breaker", Until}` por excluido |
| `rankAttemptsByScore` ([router_scoring.go:230](../../internal/router/router_scoring.go)) | el score de cada modelo, ya calculado ahí — sin segunda pasada |
| tras el truncado a `MaxAttempts` (:307) | `Eligible` |
| `executeAttempt`, cada camino de fallo ([router_upstream.go](../../internal/router/router_upstream.go)) | un `traceAttempt` por fallo, con el estatus **crudo** del upstream |
| `executeAttempt`, éxito no-stream ([router_upstream.go:252](../../internal/router/router_upstream.go)) | `Served` |
| `executeAttempt`, éxito stream (:209) | `Served`, antes de `setServedModelHeaders` |
| `setServedModelHeaders` ([router_http.go:76](../../internal/router/router_http.go)) | materializa `X-Calvoproxy-Route` y `X-Calvoproxy-Decision-Id` |
| las cuatro salidas sin servido (:282, :299, :339, :366) | `Outcome` + materializa la traza parcial antes de `writeJSONError` |

**La traza viaja en el `context.Context` con clave tipada.** No como campo de
`FallbackExecution`: `setServedModelHeaders` se llama desde **dentro** de `executeAttempt`
(`router_upstream.go:209` y `:259`), y `AttemptExecutor.ExecuteAttempt`
([router_types.go:157](../../internal/router/router_types.go)) no recibe `FallbackExecution`.
Verificado.

Accesor `traceFrom(ctx) *routeTrace`, que devuelve `nil` fuera de banda. Todos los métodos de
anotación son no-op sobre receptor `nil`, siguiendo el patrón de `s.capabilities != nil`
([router.go:618](../../internal/router/router.go)).

### 4.1 Concurrencia

Un solo escritor: la goroutine del request. `streamCopy`
([router_stream.go:97](../../internal/router/router_stream.go)) y `awaitFirstStreamEvent`
(`:236`) **no** tocan la traza. El desenlace del stream se consolida en el `switch` de
`router_upstream.go:213-229`, que corre en la goroutine del request — pero **después** de que
las cabeceras estén enviadas, así que no puede reflejarse en el header. Sí se refleja en la
copia del ring, y por tanto en `/decisions/{id}`.

Al cerrar el request se copia una versión compactada al ring. Nunca se publica el puntero vivo.

## 5. Ring y `/decisions/{id}`

Ring circular en memoria en `RouterService`, tamaño fijo `PROXY_TRACE_RING` (defecto 200),
con su propio mutex. Nunca se persiste a disco: son cuerpos de conversación.

`GET /decisions/{id}` bajo el mismo gate `admin` que `/health`
([cmd/main.go:119](../../cmd/main.go)). Devuelve el JSON completo, **con** `Reason`.
`404` si el id ya rotó fuera del ring.

## 6. Saneamiento y canales

Tres canales con contenido distinto. La regla es que **`Reason` solo existe en el canal
admin**, porque es cuerpo de error del upstream (`truncateReason` lo corta a 240 bytes,
[router_breaker.go:559](../../internal/router/router_breaker.go), pero no lo sanea).

| Canal | Gate | Contiene |
|---|---|---|
| `X-Calvoproxy-Route` | ninguno | forma corta; solo códigos y enumerados |
| `X-Calvoproxy-Trace: full` (opt-in del cliente) | ninguno | JSON completo **sin** `Reason` |
| `GET /decisions/{id}` | `admin` | JSON completo **con** `Reason` truncado |

Todo valor que entre en el header se filtra a `[A-Za-z0-9._/+-]`; cualquier otro byte se
sustituye por `_`. Un nombre de modelo viene de la config, pero `RequestedModel` viene del
cliente y no puede inyectar CR/LF ni separadores.

## 7. Desactivación

`PROXY_ROUTE_TRACE=off` → `dispatchChain` no crea traza, `traceFrom` devuelve `nil`, no se
emite ninguna cabecera nueva y no se toca el ring. El comportamiento observable vuelve a ser
byte a byte el actual (invariante 2).

## 8. Invariantes verificables

| # | Invariante | Cómo se prueba |
|---|---|---|
| 1 | El header sale **antes** del primer byte del cuerpo, también en SSE | `headerSnapshotRecorder` ([router_critical_path_test.go:68](../../internal/router/router_critical_path_test.go)), como `TestStreaming_ServedModelHeadersPrecedeTheBody` |
| 2 | Con `PROXY_ROUTE_TRACE=off` nada observable cambia | mismo request con y sin la env; se comparan estatus, cuerpo y conjunto de cabeceras |
| 3 | Traza `nil` es no-op en todos los puntos de anotación | llamada directa a cada método sobre receptor `nil`, sin panic |
| 4 | El header nunca supera 512 bytes | cadena con el máximo de modelos, todos fallando, nombres y razones máximos; se afirma `len <= 512` y `trunc=1` |
| 5 | Las cuatro salidas sin servido emiten traza parcial con su `o=` | un test por camino: 422 capacidad pinada, 503 capacidad, 503 all-cooling, fallo de cadena |
| 6 | gRPC hereda el header sin trabajo nuevo en el router | request gRPC; se afirma `X-Calvoproxy-Route` en el mapa `Headers` de la respuesta |
| 7 | Valor único: la cabecera no se duplica al copiar las del upstream | upstream que emite `X-Calvoproxy-Route`; se afirma un solo valor tras `streamProxyResponse` |
| 8 | Sin carrera bajo `-race` con streaming concurrente | N requests en paralelo, `go test -race` |

El invariante 7 no estaba en el goal: lo añade esta spec porque `streamProxyResponse` hace
`copyHeaders` con `dst.Add` ([router_http.go:58](../../internal/router/router_http.go))
**después** de que `setServedModelHeaders` haya escrito las nuestras. Un upstream que emitiese
la misma cabecera produce dos valores. Hay que añadir `x-calvoproxy-route` y
`x-calvoproxy-decision-id` a los `skipKeys` de esa llamada.

> **Corrección (fase B).** El primer borrador de esta spec afirmaba que en ese escenario
> "ganaría el del upstream", porque `cmd/grpc.go:104` toma `values[0]`. Es **falso**, y se
> comprobó ejecutando el test: los valores observados son
> `["v1;p=simple;cmp=off", "v1;p=INJECTED"]` — el nuestro va primero, porque
> `setServedModelHeaders` se ejecuta antes que el `copyHeaders`. gRPC, por tanto, ya toma el
> correcto.
>
> El fallo real es la **duplicación**: el cliente recibe dos valores de la misma cabecera, el
> segundo con contenido que controla el upstream. Un cliente que lea todos los valores, o que
> se quede con el último, o cuyo stack los una con comas, consume texto ajeno como si fuera
> una decisión de routing del proxy. El invariante se mantiene sin cambios —un solo valor, y
> que el del upstream no sobreviva— pero su justificación era equivocada.
>
> Esto ya afecta hoy a `X-Calvoproxy-Model`, `-Profile` y `-Attempt`, que tienen exactamente
> el mismo problema. Queda **fuera del alcance de P1**: el `skipKeys` de esta spec cubre solo
> las dos cabeceras nuevas, y las tres antiguas merecen su propio cambio y su propio test.

> **Corrección (fase B).** El borrador situaba la anotación de fallos en el bucle de
> `DefaultFallbackExecutor.Execute`. Es el sitio equivocado: cuando el error llega al bucle,
> `classifyHTTPError` ya lo ha pasado por `cervoretry.ClassifyHTTPStatus`, que **remapea** —
> un 500 del upstream llega al bucle como 502. Una traza que reporte `model-a:502` cuando
> OpenRouter dijo 500 despista justo a quien intenta averiguar qué pasó de verdad.
> `executeAttempt` es el único punto que ve `resp.StatusCode` sin remapear, así que la
> anotación vive ahí. El bucle no anota nada.

## 9. Fuera de alcance

El desenlace del stream (`streamCompleted` / abortado) no puede estar en el header: se conoce
después de enviarlo. Vive solo en el ring y en `/decisions/{id}`. Cualquier consumidor que
necesite el desenlace consulta ese endpoint; no se romperá el contrato del header para
metérselo.
