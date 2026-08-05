# P1 — progreso

Un renglón por incremento. Invariantes numerados según
[P1-decision-trace.md §8](P1-decision-trace.md).

| # | Invariante | Estado | Nota |
|---|---|---|---|
| 1 | Header antes del primer byte, también SSE | ✅ | Rojo primero. Usa `headerSnapshotRecorder` |
| 2 | `PROXY_ROUTE_TRACE=off` no cambia nada observable | ✅ | Rojo primero. Off ⇒ no se asigna traza |
| 3 | Traza `nil` es no-op | ✅ | **Sin rojo previo**: la seguridad ante `nil` ya estaba en el diseño del incremento 1. Test de caracterización |
| 4 | Header ≤ 512 bytes con recorte determinista | ✅ | Rojo primero |
| 5 | Las cuatro salidas sin servido emiten traza parcial | ✅ | Rojo primero, un subtest por camino |
| 6 | gRPC hereda la cabecera | ✅ | Rojo primero, en `cmd/grpc_test.go` |
| 7 | Cabecera de valor único | ✅ | Rojo primero |
| 8 | Sin carreras bajo `-race` con streaming concurrente | ✅ | Rojo primero — ver abajo |
| 9 | Opt-in `full` sin `Reason` | ✅ | Rojo primero |
| 10 | `/decisions/{id}` con `Reason`, tras `admin` | ✅ | Rojo primero |
| 11 | Ring acotado, descarta lo más viejo | ✅ | Rojo primero |

Puertas: `go build -mod=vendor` · `go test -race ./...` · `coverage-gate.sh` — las tres en
verde.

## Donde la spec estaba equivocada

- **§8, invariante 7.** El borrador decía que ante una cabecera duplicada "ganaría la del
  upstream" porque `cmd/grpc.go` toma `values[0]`. Falso: la nuestra se escribe antes, así que
  va primero. El fallo real es la duplicación en sí, con texto del upstream en el segundo
  valor.
- **§4, punto de anotación de fallos.** El borrador lo situaba en el bucle de fallback. Es el
  sitio equivocado: `cervoretry.ClassifyHTTPStatus` remapea antes de que el error llegue ahí
  (500 → 502), así que la traza habría reportado un código que el upstream nunca envió. La
  anotación se movió a `executeAttempt`, el único punto que ve `resp.StatusCode` crudo.

## Lo que encontró el invariante 8

La primera ejecución dio tres `DATA RACE`, pero **no en el código de la traza**: el helper
compartido `streamTransport` ([router_critical_path_test.go:32](../../internal/router/router_critical_path_test.go))
incrementa `calls` y escribe `lastURL` sin lock. Ningún test lo compartía entre goroutines
hasta ahora. El test de P1 usa un transport sin estado; el helper sigue con la carrera latente
y merece su propio arreglo — fuera del alcance de P1.

## Cierre

Los once invariantes en verde, las tres puertas pasando, load test sin cambios, CHANGELOG y
README actualizados.

§5 y §6 (ring, `/decisions/{id}`, opt-in `full`) llegaron en un último incremento con sus
propios invariantes 9–11. Se planteó recortarlos a P5 —donde el ring hace falta igualmente
para el dashboard— pero recortar alcance sin que nadie lo decida es peor que hacer el trabajo,
así que se implementaron.

## Pendiente de decisión humana

- Formato del header: quedará congelado como API en cuanto Hermes lo parsee. Sigue sin
  aprobación explícita; se avanzó sobre el borrador.

## Fuera de alcance detectado por el camino

- `X-Calvoproxy-Model`, `-Profile` y `-Attempt` sufren la misma duplicación que arreglamos
  para las cabeceras nuevas.
- La carrera del helper `streamTransport`.
