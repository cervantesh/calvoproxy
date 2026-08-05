# P5 — Local dashboard

Reference architecture: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md). Consumes P1 (trace and
ring), P2 (quotas) and the existing counters.

## 1. Problem

The proxy already computes everything interesting — scores, circuits, quotas, routing
decisions — and exposes it in three places you have to read separately and by hand:
`/health`, `/metrics` and `/decisions/{id}`, the last one only if you already know the id.
There is nowhere to answer "what is happening right now?".

## 2. Scope: it is a view, not a subsystem

**The dashboard computes nothing.** Every aggregate it shows must already exist as a router
snapshot, the same way `/metrics` works. If something is needed and does not exist, it gets
added to the router and tested there — not in the presentation layer.

- `embed.FS` with plain HTML and JS. **No Node, no build step, no framework**: the binary has
  to keep compiling offline with `-mod=vendor`.
- **Polling every 2 s, no WebSockets.** For a local single-user tool, a websocket hub is a
  second streaming path inside the binary just to paint a table.
- **No historical series.** `/metrics` with Prometheus already does that. This is "state now +
  the last N decisions".

## 3. Surface

| Route | Gate | What it returns |
|---|---|---|
| `GET /dashboard` | `admin` | the embedded HTML |
| `GET /dashboard/state` | `admin` | JSON: `Health()` + `Counters()` + quotas + the last N traces |

Both behind the same `admin` gate as `/health` ([cmd/main.go:119](../../cmd/main.go)), because
they show exactly what that gate protects: model chains, upstream error text and the router's
internal state.

Since the channel is admin, traces are served **with** `Reason` — the same rule as
`/decisions/{id}` (P1 §6). The gate authorises, not the path.

## 4. What has to be added to the router

One thing only: `traceRing.recent(n)`, which does not exist today — the ring can only look up
by id. It reads under `RLock`, returns the **newest first**, and honours the requested limit.

## 5. Verifiable invariants

| # | Invariant | How it is tested |
|---|---|---|
| 1 | With `PROXY_ADMIN_TOKEN` set, both routes require the token | request without a token → 401 |
| 2 | `recent(n)` returns newest first and honours the limit | ring with more entries than the limit |
| 3 | `recent` on an empty or nil ring returns empty, does not blow up | freshly created ring, and nil |
| 4 | `/dashboard/state` includes health, counters, quotas and decisions | one served request; assert all four keys |
| 5 | The page is self-contained: not one external resource | assert the HTML references neither `http://` nor `https://` |
| 6 | The HTML is served as `text/html` and the state as `application/json` | Content-Type of both |

## 6. Out of scope

Its own authentication (it uses the existing gate), editing configuration from the web (it is a
read-only view, and writing configuration from a local browser would open a surface this
project does not need), and any time-series charting.
