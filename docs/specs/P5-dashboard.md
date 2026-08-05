# P5 — Dashboard local

Arquitectura de referencia: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md). Consume P1 (traza y
ring), P2 (cuotas) y los contadores existentes.

## 1. Problema

El proxy ya calcula todo lo interesante —scores, circuitos, cuotas, decisiones de routing— y
lo expone en tres sitios que hay que leer a mano y por separado: `/health`, `/metrics` y
`/decisions/{id}`, este último solo si ya conoces el id. No hay ningún sitio donde mirar
"¿qué está pasando ahora?".

## 2. Alcance: es una vista, no un subsistema

**El dashboard no calcula nada.** Todo agregado que muestre debe existir antes como snapshot
del router, igual que `/metrics`. Si algo hace falta y no existe, se añade al router y se
prueba allí — no en la capa de presentación.

- `embed.FS` con HTML y JS a pelo. **Sin Node, sin build, sin framework**: el binario tiene que
  seguir compilando offline con `-mod=vendor`.
- **Polling cada 2 s, sin WebSockets.** Para una herramienta local de un solo usuario, un hub
  de websockets es un segundo camino de streaming dentro del binario para pintar una tabla.
- **Sin series históricas.** Para eso ya está `/metrics` con Prometheus. Esto es "estado ahora
  + últimas N decisiones".

## 3. Superficie

| Ruta | Gate | Qué devuelve |
|---|---|---|
| `GET /dashboard` | `admin` | el HTML embebido |
| `GET /dashboard/state` | `admin` | JSON: `Health()` + `Counters()` + cuotas + últimas N trazas |

Ambas bajo el mismo `admin` que `/health` ([cmd/main.go:119](../../cmd/main.go)), porque
enseñan exactamente lo que ese gate protege: cadenas de modelos, texto de error del upstream y
el estado interno del router.

Como el canal es admin, las trazas se sirven **con** `Reason` — la misma regla que
`/decisions/{id}` (P1 §6). Es el gate lo que autoriza, no la ruta.

## 4. Lo que hay que añadir al router

Una sola cosa: `traceRing.recent(n)`, que hoy no existe — el ring solo sabe buscar por id. Es
lectura bajo `RLock`, devuelve **las más recientes primero**, y respeta el límite pedido.

## 5. Invariantes verificables

| # | Invariante | Cómo se prueba |
|---|---|---|
| 1 | Con `PROXY_ADMIN_TOKEN` puesto, ambas rutas exigen el token | petición sin token → 401 |
| 2 | `recent(n)` devuelve las más recientes primero y respeta el límite | ring con más entradas que el límite |
| 3 | `recent` sobre un ring vacío o nil devuelve vacío, no revienta | ring recién creado y nil |
| 4 | `/dashboard/state` incluye salud, contadores, cuotas y decisiones | un request servido; se afirman las cuatro claves |
| 5 | La página es autocontenida: ni un solo recurso externo | se afirma que el HTML no referencia `http://` ni `https://` |
| 6 | El HTML se sirve con `text/html` y el estado con `application/json` | Content-Type de ambas |

## 6. Fuera de alcance

Autenticación propia (usa el gate que ya hay), edición de configuración desde la web (es una
vista de solo lectura, y escribir configuración desde un navegador local abriría una
superficie que este proyecto no necesita), y cualquier gráfica de series temporales.
