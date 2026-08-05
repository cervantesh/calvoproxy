# P5 — progreso

- **Invariantes 1–6 en verde.** Ocho tests entre `cmd/dashboard_test.go` y
  `internal/router/router_trace_recent_test.go`. Tres puertas OK.
- **Lo único que hubo que añadir al router** fue `traceRing.recent(n)`: el ring
  sabía buscar por id pero no listar. Se probó allí, no en la vista — la regla de
  que el dashboard no calcula nada se sostiene sola si lo que necesita no existe
  y hay que ir a añadirlo donde sí hay tests.
- **Decisión no prevista en la spec**: la cabecera `Content-Security-Policy` con
  `default-src 'self'`. No estaba pedida, pero convierte el invariante 5 ("nada
  externo") en algo que también falla en el navegador y no solo en un test que
  alguien podría borrar.
- Pendiente: la columna de compresión aparecerá sola cuando P3 rellene `cmp=`.
