# Load / stress harness

Two ways to load-test CalvoProxy:

## 1. CI regression gate (automatic)

`cmd/load_test.go` (`TestLoad_NoDeadlockUnderConcurrency`) runs as part of
`go test ./...`. It drives a real HTTP server wrapping the proxy against a mock
upstream (with injected failures) under concurrency, while hammering `/health`,
and asserts the invariants that must never regress:

- no deadlock / crash (hard 60s failsafe),
- **zero** client transport errors (guards the upstream connection-pool fix),
- **zero** failed `/health` polls (guards the breaker/Health locking fix),
- ≥90% success via the fallback chain.

Scale it up for a quick in-process benchmark:

```bash
LOAD_N=25000 LOAD_C=200 go test ./cmd -run TestLoad -v
```

## 2. Standalone harness (heavy / real-upstream benchmarking)

For big numbers, or to point at a **real** running proxy / real OpenRouter,
use the two standalone tools here.

**Mock upstream** — fast local stand-in for OpenRouter (configurable failure
injection, SSE support):

```bash
MOCK_ADDR=127.0.0.1:29900 MOCK_FAIL_PCT=15 go run ./test/load/mock
```

**Load generator** — fires N requests over C workers (mixed stream/non-stream)
and polls `/health` concurrently; reports throughput, latency percentiles and
the status distribution:

```bash
LOAD_URL=http://127.0.0.1:8080 LOAD_C=200 LOAD_N=25000 LOAD_STREAM_PCT=25 \
  go run ./test/load/loadgen
```

### Two-layer method

- **Layer 1 — synthetic (mock upstream):** start the mock, start a proxy with
  `PROXY_OPENROUTER_URL=http://127.0.0.1:29900/v1/chat/completions`, then run the
  load generator against the proxy. Isolates the proxy from network/quota so you
  measure the proxy itself and exercise breaker/scoring/fallback via injected
  failures.
- **Layer 2 — real (OpenRouter):** point the load generator at your real proxy
  (`:8080`) at modest concurrency to observe genuine free-tier rate-limit
  handling (graceful `503` via the fallback chain, honoring `Retry-After`).

### Reference baseline

Local synthetic run (200 workers, 25k requests, 25% streaming, 15% injected
upstream failures), after the connection-pool fix — keep these as a regression
baseline:

| Metric | Value |
|---|---|
| Throughput | ~7,500 req/s |
| Success | 99.9% (via fallback) |
| Transport errors | 0 |
| p50 / p99 / max | ~21 / ~156 / ~305 ms |
| Proxy OS threads | ~64 |
| `/health` polls failed | 0 |

Env knobs that matter under load: `PROXY_MAX_IDLE_CONNS_PER_HOST` (upstream
pool), `PROXY_MAX_CONCURRENT` (admission cap), the breaker/timeout settings.
