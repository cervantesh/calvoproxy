# P1 — progreso

Un renglón por incremento. Invariantes numerados según
[P1-decision-trace.md §8](P1-decision-trace.md).

| # | Invariante | Estado | Nota |
|---|---|---|---|
| 1 | Header antes del primer byte, también SSE | ✅ | Rojo primero. Usa `headerSnapshotRecorder` |
| 2 | `PROXY_ROUTE_TRACE=off` no cambia nada observable | ✅ | Rojo primero. Off ⇒ no se asigna traza |
| 3 | Traza `nil` es no-op | ✅ | **Sin rojo previo**: la seguridad ante `nil` ya estaba en el diseño del incremento 1. Test de caracterización |
| 4 | Header ≤ 512 bytes con recorte determinista | ⏳ | Necesita `prev=` y `trunc=1` |
| 5 | Las cuatro salidas sin servido emiten traza parcial | ⏳ | Necesita `Outcome` y `o=` |
| 6 | gRPC hereda la cabecera | ⏳ | |
| 7 | Cabecera de valor único | ✅ | Rojo primero |
| 8 | Sin carreras bajo `-race` con streaming concurrente | ⏳ | |

## Donde la spec estaba equivocada

- **§8, invariante 7.** El borrador decía que ante una cabecera duplicada
  "ganaría la del upstream" porque `cmd/grpc.go` toma `values[0]`. Falso: la nuestra se
  escribe antes, así que va primero. El fallo real es la duplicación en sí, con texto del
  upstream en el segundo valor. Corregido en la spec con la evidencia del test.

## Pendiente de decisión humana

- Formato del header: quedará congelado como API en cuanto Hermes lo parsee. Sigue sin
  aprobación explícita; se avanza sobre el borrador.

## Fuera de alcance detectado por el camino

- `X-Calvoproxy-Model`, `-Profile` y `-Attempt` sufren la misma duplicación que arreglamos
  para las cabeceras nuevas. Merece su propio cambio y su propio test.
