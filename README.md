# CalvoProxy

*English · [Español](README.es.md)*

Smart OpenAI-compatible proxy that fronts free OpenRouter models behind one
endpoint. It applies deterministic request policy, selects a model chain per
request, and adds gateway concerns — timeouts, retries, circuit breaking,
reliability scoring, limits and audit — on top of upstream forwarding.

CalvoProxy is **fully self-contained**: all of its dependencies are **vendored**
(`vendor/`), so it builds and runs offline with no external module access.

## Quickstart

Three commands from nothing to a working proxy:

```bash
calvoproxy login     # browser sign-in with OpenRouter; stores a per-user key
calvoproxy           # starts on http://127.0.0.1:8080
calvoproxy doctor    # verifies the whole chain and your client wiring
```

`doctor` is the shortcut past every trap below. It checks, in the order you
actually hit them: the proxy answers, credentials resolve, a **real completion
survives the whole chain**, and — if Hermes is installed — that Hermes is wired
to route through the proxy. Every failure prints the fix, and a failing run
prints the exact config block to paste. Use `--no-live` to skip the real
request.

### Wiring Hermes (read this before editing config.yaml)

Two keys are required, and missing either one fails **silently** — Hermes keeps
working, it just quietly talks to OpenRouter (or your previous provider)
instead of the proxy:

```yaml
model:
  provider: custom
  default: coding
  base_url: http://127.0.0.1:8080/v1   # REQUIRED — see below
  api_key: dummy                       # the real key lives in the proxy

custom_providers:
  - name: calvoproxy
    base_url: http://127.0.0.1:8080/v1  # must match model.base_url exactly
    api_key: dummy
    api_mode: chat_completions
    discover_models: false              # the proxy serves no /v1/models
    models:
      coding:    {context_length: 131072}
      simple:    {context_length: 131072}
      reasoning: {context_length: 131072}
      vision:    {context_length: 131072}
```

- **`model.base_url` is what binds `provider: custom` to the `custom_providers`
  entry.** Without it Hermes resolves `provider: custom` but keeps
  `base_url: https://openrouter.ai/api/v1`, then sends a profile name like
  `coding` upstream and gets `400: coding is not a valid model ID`.
- **Use `127.0.0.1`, not `localhost`.** Hermes only trusts `model.base_url`
  when the host is loopback, and `localhost` can resolve to IPv6 `::1` while
  the proxy listens on IPv4 only.
- **The gateway does not reload `config.yaml` while running.** Restart it after
  editing, or nothing changes.

Confirm it actually took effect — the proxy's own counters are the only proof:

```bash
curl -s http://127.0.0.1:8080/metrics | grep calvoproxy_requests_by_status
```

Send one message through Hermes and watch `class="2xx"` increase. If it does
not move, the request never reached the proxy.

## Build

```bash
go build -mod=vendor -o calvoproxy ./cmd
```

The build is fully offline — every dependency lives under `vendor/`. You can
prove it with `GOPROXY=off go build ./cmd`.

> Because dependencies are vendored (some originate from a private registry that
> is not publicly resolvable), builds always use `vendor/`. Do **not** run
> `go mod tidy` / `go get` here — it would try to re-resolve those private
> modules over the network. To change dependencies, work in the source monorepo
> and re-vendor.

## Run

```bash
./calvoproxy
```

The server exposes an OpenAI-compatible HTTP API. Streaming (`stream: true`)
is piped through with flushing; `SIGINT`/`SIGTERM` drain in-flight requests
before exit.

| Env var              | Default | Description                          |
|----------------------|---------|--------------------------------------|
| `HOST`               | `127.0.0.1` | Bind address. Loopback by default (keeps the proxy and its env key off the network). Set `0.0.0.0` to expose it — the Docker image does this automatically |
| `PORT`               | `8080`  | HTTP listen port                     |
| `GRPC_PORT`          | `9090`  | gRPC listen port (see [gRPC](#grpc-transport)); a bind failure is non-fatal |
| `OPENROUTER_API_KEY` | —       | Upstream key for the default executor|
| `PROXY_IDLE_TIMEOUT` | off     | Exit after this idle period (Go duration, e.g. `20m`) — enables on-demand use |
| `PROXY_MAX_BODY_BYTES` | `10485760` | Max request body (10 MiB) — guards against oversized payloads |
| `PROXY_MAX_RESPONSE_BYTES` | `26214400` | Max buffered non-streaming upstream response (25 MiB) — guards against OOM |
| `PROXY_REQUEST_TIMEOUT_SECONDS` | `45` | Per-attempt timeout (one upstream call). Header arrival for streams is bounded by this too |
| `PROXY_TOTAL_TIMEOUT_SECONDS` | `120` | Overall wall-clock budget across the fallback chain (non-streaming) |
| `PROXY_STREAM_IDLE_TIMEOUT` | `120` | Max gap (seconds) between streamed chunks before a stalled stream is aborted |
| `PROXY_STREAM_MAX_DURATION` | `1800` | Absolute cap (seconds) on a single stream's lifetime; `0` disables the backstop |
| `PROXY_MAX_IDLE_CONNS_PER_HOST` | `128` | Idle-connection pool size per upstream host. Raising it above the stdlib default of 2 avoids connection churn under concurrency |
| `PROXY_MAX_CONCURRENT` | off | Cap on concurrent in-flight requests. A burst over the cap waits, then gets `503 Retry-After`; keeps a spike from stampeding the upstream past its rate limits |
| `PROXY_ADMISSION_TIMEOUT_SECONDS` | `5` | How long an over-cap request waits for a slot before `503` (only when `PROXY_MAX_CONCURRENT` is set) |
| `PROXY_SCORING_ENABLED` | `true` | Reorder the chain by per-model reliability score (see below) |
| `PROXY_SCORING_RECOVERY_SECONDS` | `21600` (6 h) | Wall-clock half of the score decay window: how long a demoted model takes to be fully forgiven |
| `PROXY_SCORING_RECOVERY_ATTEMPTS` | `50` | Evidence half of the same window: how many further scored attempts, proxy-wide, it takes. Decay advances at the **slower** of the two, so an idle proxy does not forget |
| `PROXY_SCORE_FILE` | `<user-config-dir>/calvoproxy/scores.json` | Where learned scores are persisted across restarts. Set to `off` to disable persistence |
| `PROXY_SCORE_MAX_AGE_SECONDS` | `86400` (24 h) | Discard a persisted score file (and individual entries) older than this |
| `PROXY_BREAKER_FAILURE_THRESHOLD` | `3` | Consecutive failures before a model's circuit opens |
| `PROXY_BREAKER_COOLDOWN_SECONDS` | `60` | How long an open circuit skips a model |
| `PROXY_OPENROUTER_URL` | OpenRouter | Override the OpenRouter chat endpoint (e.g. a mock) |
| `PROXY_AGENTIC_URL`  | off     | If set, `agent`/`plan` profiles route here; unset → normal OpenRouter routing |
| `PROXY_WORKSPACE_SIDE_EFFECTS` | `false` | Opt-in monorepo git/sqlite extractor (off by default) |
| `PROXY_ADMIN_TOKEN`  | off     | If set, gates `/health`, `/metrics`, `/health/model-policy`, `/admin/reload` behind a Bearer token (constant-time) |
| `PROXY_METRICS_TOKEN` | off    | If set, `/metrics` accepts this token OR the admin token — decouples the scraper credential from admin |
| `PROXY_ALLOW_ENV_KEY_PUBLIC` | `false` | Allow spending the env `OPENROUTER_API_KEY` for keyless requests on a **public** bind (loopback always allows it) |
| `PROXY_OAUTH_REQUIRE_STATE` | `true` | Require a matching CSRF `state` on the `calvoproxy login` callback. OpenRouter echoes it, so this is on by default; set `false` only for a provider that doesn't (the secret callback path + PKCE still apply) |
| `PROXY_UPDATE_CHECK` | `true`  | Startup check for a newer release (logs a recommendation). Set `false` to disable |
| `PROXY_ALLOW_PAID_EMBEDDINGS` | `false` | Allow `/v1/embeddings`, which bills real credit — OpenRouter has no free embedding model, and that path has no chain/breaker/fallback |
| `PROXY_MODEL_*`, `PROXY_POLICY_*` | — | Accepted as aliases for the legacy `CERVO_MODEL_*` / `CERVO_POLICY_*` names, which the vendored policy library still reads. The legacy name wins when both are set |

Prometheus metrics are at **`/metrics`** (per-model score, consecutive failures,
successes, open-circuit count, readiness, plus request rate, per-status-class
counts, latency sum/count, stream outcomes (`completed`/`stalled`/
`upstream_error`/`max_duration`/`client_gone`), admission rejections,
capability refusals, gRPC request count and a `build_info` gauge). When `PROXY_ADMIN_TOKEN` is
set, the detailed endpoints require it; `/ready` stays open and returns
readiness only.

> **Secure defaults.** `HOST` is loopback (`127.0.0.1`) by default, and the
> admin/metrics/health endpoints are open only on that loopback bind. If you
> expose the proxy (`HOST=0.0.0.0`, or via Docker), **set `PROXY_ADMIN_TOKEN`** —
> otherwise those endpoints are world-readable. On a public bind the proxy also
> refuses to spend the env `OPENROUTER_API_KEY` for keyless requests unless
> `PROXY_ALLOW_ENV_KEY_PUBLIC=true`, so an exposed instance can't become an open
> relay on your dime. A startup warning fires if you expose it without a token.

**Hot-reload** the model chains without a restart: edit `model-policy.json`, then
`kill -HUP <pid>` (Unix) or `POST /admin/reload` (any platform, admin-gated).

### Capability-aware routing (vision + tools)

Not every free model accepts images or does tool calling. CalvoProxy tags each
model with its capabilities and, when a request needs one, filters the chain to
models that actually support it before breaker/scoring/fallback — so a photo goes
to a vision model and a tool-calling request goes to a tool-capable model (and a
request that needs **both** goes to a model that does both).

- **Detection:** image content ⇒ needs `vision`; a `tools`/`functions` array ⇒
  needs `tools`. Plain text requests are never filtered (zero change).
- **Fail-closed:** a model with no known capability data does **not** qualify, so
  images/tools are never silently routed to an incapable model. If you pin a
  specific `model` that can't do what the request needs, you get a clear `422`.
- **Rescue:** if the selected profile has no capable model, the router falls back
  to any known-capable model across profiles; if none exists, a clear `503`.
- **Capabilities source (hybrid):** auto-derived from the public OpenRouter
  `/models` API (`input_modalities`/`supported_parameters`), **merged with** manual
  overrides in `model-policy.json` (authoritative — use `"!vision"`/`"!tools"` to
  deny a wrongly-reported capability):

  ```json
  "Capabilities": {
    "google/gemma-4-31b-it:free": ["vision", "tools"],
    "openai/gpt-oss-20b:free": ["tools"]
  }
  ```

  Auto-derive runs in the background (`PROXY_CAPABILITY_AUTODERIVE=false` to
  disable; `PROXY_CAPABILITY_REFRESH_SECONDS` to change the 6h interval); the
  curated overrides cover the chain models synchronously so it works offline too.
  Refresh the capability data the same way as the model list — the OpenRouter
  `/models` response carries `architecture.input_modalities` and
  `supported_parameters` per model.

### Reliability: circuit breaker + scoring

Two layers keep flaky models out of the way:

- **Circuit breaker** (hard gate): after `PROXY_BREAKER_FAILURE_THRESHOLD`
  consecutive failures a model's circuit **opens** and it is skipped entirely
  for `PROXY_BREAKER_COOLDOWN_SECONDS`; a success closes it.
- **Reliability score** (soft ranking): every model carries a score in `[0,1]`
  that rises on success and falls on failure — harder for rate-limits (429),
  server errors (5xx) and timeouts, and it also drops for "model unavailable"
  404s. The eligible chain is **reordered by score** (most reliable first)
  before each request, so a struggling model sinks to the back without being
  removed, and recovers toward a neutral baseline so it gets retried later.
  Scores are visible under `circuits[].score` in `/health` and as
  `calvoproxy_model_score` in `/metrics`. Set `PROXY_SCORING_ENABLED=false` to
  keep the static chain order.

**How a score recovers.** Decay is measured against two clocks and advances at
the **slower** of them: elapsed wall time (`PROXY_SCORING_RECOVERY_SECONDS`,
default 6 h) and the proxy-wide count of scored attempts
(`PROXY_SCORING_RECOVERY_ATTEMPTS`, default 50). The attempt clock is the one
that matters: nothing about a model changes while nobody is calling it, so an
idle gap must not erase what was learned. Only further attempts — real new
evidence, for or against — move a score back toward its baseline.

Before v0.9.2 decay was pure wall-clock over a five-minute window, which made
the whole subsystem inert on a bursty, interactive workload: every score
converged on the neutral baseline during any gap between sessions, the chain was
re-ranked into exactly its configured order, and the next burst re-paid the
whole discovery cost. On a live instance that showed up as 29% of requests
abandoned at the first-event budget, and as a model with 96 successes scoring
identically (0.8000) to one with 0 successes and a standing failure.

**Never-succeeded models.** A model that has not once succeeded in the proxy's
memory drifts back to a lower baseline (`0.5`) than one that merely had a bad
spell (`0.8`). Zero successes is not the same evidence as a bad day: a bad day
is contradicted by the successes around it. Such a model is still tried — last —
and a single real success moves it onto the normal baseline for good.

**Persistence.** Scores are written to `PROXY_SCORE_FILE` (default
`<user-config-dir>/calvoproxy/scores.json`) every 30 s when they change, and
once more on a clean shutdown; they are reloaded at startup. Before v0.9.2 the
score map was in-memory only, so every restart — including installing a new
build — discarded everything the proxy had learned. Deliberate limits:

- **Breaker state is not persisted.** A cooldown is a statement about right now,
  and a restart is a good reason to re-probe. Only the score, its two clock
  readings, and the success count survive.
- **Stale files are discarded** whole, along with individual entries older than
  `PROXY_SCORE_MAX_AGE_SECONDS` (default 24 h). Free-tier slugs get retired and
  re-provisioned on that timescale.
- **Keys the current policy no longer names are dropped** on load. Editing a
  chain is exactly the signal that the model set changed, and a key that is in
  no chain can only be reached by an explicit pin, where the chain is one model
  long and the score orders nothing.
- Set `PROXY_SCORE_FILE=off` to turn persistence off entirely. Nothing is ever
  written to the working directory: if no config directory can be determined,
  persistence simply stays off.
- **In a container, mount a volume.** The image pins
  `PROXY_SCORE_FILE=/data/scores.json` and creates `/data` world-writable so it
  works under `--user` too; without a volume mounted there, scores are lost on
  every recreation. Compose declares one for you.

### Capacity & tuning

CalvoProxy itself is not the bottleneck — a load test (200 workers, 25k requests
against a fast upstream) sustained **~7,500 req/s at p99 ~156 ms with zero
transport errors**, and `/health` stayed responsive throughout. The practical
limit for real workloads is the **upstream's** rate limit: OpenRouter's free tier
rate-limits well before the proxy breaks a sweat, so a burst of ~30 concurrent
free-tier requests mostly degrades to a clean `503` (via the fallback chain,
honoring upstream `Retry-After`). For Hermes/Claude-Code-style low-concurrency use
you have enormous headroom.

Tuning under load:

- **`PROXY_MAX_IDLE_CONNS_PER_HOST`** (default 128) — the single most important
  knob at high concurrency; too low means connection churn (port/thread
  exhaustion), too high wastes sockets.
- **`PROXY_MAX_CONCURRENT`** — set it to smooth bursts: instead of stampeding the
  upstream (and collapsing the chain to 503s), excess requests wait up to
  `PROXY_ADMISSION_TIMEOUT_SECONDS` then get a `503 Retry-After`.
  A slot is held for the **whole** request, streams included (that is the point —
  a live stream still occupies an upstream connection), so raise the cap on
  stream-heavy deployments.
- Breaker/timeout knobs (`PROXY_BREAKER_*`, `PROXY_REQUEST_TIMEOUT_SECONDS`,
  `PROXY_TOTAL_TIMEOUT_SECONDS`) shape how aggressively a flaky upstream is shed.

**Alerting** — the `/metrics` counters map to the usual SLO alerts: page on a
sustained rise in `calvoproxy_requests_by_status{class="5xx"}` relative to
`calvoproxy_requests_total` (chain exhaustion / upstream down), on
`calvoproxy_open_circuits > 0` persisting (models stuck open), and on the derived
average latency (`calvoproxy_request_latency_seconds_sum / _count`) crossing your
budget. `calvoproxy_build_info{version=...}` labels the running build.

Reproduce or extend these measurements with the harness in
[`test/load/`](test/load/); a slimmed version runs in CI as a regression gate.

### On-demand operation

Set `PROXY_IDLE_TIMEOUT` (e.g. `20m`) and the proxy exits itself once no proxy
request arrives within the window (health/readiness probes don't count). Pair
it with a launcher that starts the proxy only when it's first needed — e.g. a
consumer's session-start hook that runs a small "start if the port is closed"
script. The proxy then runs only while it's actually in use. Left unset, the
proxy runs until killed (always-on).

Additional policy/behaviour overrides (`PROXY_DEFAULT_PROVIDER`,
`PROXY_PROVIDER_FALLBACKS_JSON`, `PROXY_LIMITS_JSON`, `PROXY_RETRY_POLICY_JSON`,
…) are documented in [docs/POLICY.md](docs/POLICY.md).

### Choosing a profile

Clients ask for a **profile name**, not a model id. The proxy resolves it to an
ordered chain and walks down that chain on failure or rate-limit.

| Profile | For | On exhaustion |
|---|---|---|
| `coding` (default) | code, and agent work that calls tools | degrades, weak tail allowed |
| `reasoning` | analysis, planning, design | degrades, but never below ~12B-class |
| `critic` | adversarial review, correctness judgments | **503 — never degrades** |
| `bulk` | summarizing, classification, first drafts | degrades freely |
| `vision` | requests containing images | vision-capable models only |

Aliases: `simple`→`bulk`, `agent`/`creative`→`coding`, `review`/`adversarial`→`critic`,
`planning`/`design`→`reasoning`.

**Profiles differ by the failure you can tolerate, not by task name.** `coding` and
`bulk` may fall through to a small model because an answer beats no answer. `critic`
may not: for a review, a confident wrong answer is worse than none, so its chain has
no weak tail and returns **503** when every member is unavailable. Retry, or escalate
to a stronger reviewer.

A profile name that does not exist is now a **404**, not a silent substitution — a
typo, or a client written against docs that shipped before the policy, must not be
answered by whatever chain prompt-classification happened to pick.

**Reading model names.** Size is in the slug, and the part that matters is the
*active* parameter count of a mixture-of-experts model:
`nemotron-3-ultra-550b-a55b` is 550B total but **55B active per token**;
`nemotron-3-nano-30b-a3b` is **3B active**. That is an 18× difference in real
compute between two models that both advertise "reasoning". `nano`, `mini`,
`xs` and `flash` mean small or latency-optimized.

**Chains degrade silently, and that is a feature with a sharp edge.** If the
first model is rate-limited the second answers, and so on. For bulk work an
answer beats no answer. For a task where a *wrong* answer is worse than no
answer — an adversarial review, a correctness judgment — a chain that falls
through to a much smaller model gives you a confident answer you should not
trust. Nothing in the HTTP status says this happened.

Two defences, both available today:

```bash
# The response names the model that actually served it.
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer dummy" \
  -d '{"model":"reasoning","messages":[{"role":"user","content":"hi"}]}' \
  | jq -r '.model'
```

- **Check the response headers.** Every answer names the model that actually
  served it, so a caller never has to guess:

  | Header | Meaning |
  |---|---|
  | `X-Calvoproxy-Model` | the model that answered |
  | `X-Calvoproxy-Profile` | the profile it was routed under |
  | `X-Calvoproxy-Attempt` | position in the chain — **anything above 1 was a fallback** |

  `X-Calvoproxy-Attempt` is the degraded signal an HTTP status can never give
  you. The body's `model` field carries the same id for callers that parse it.
- **Check `.model` on anything you intend to trust.** A profile name is a
  request, not a guarantee.
- **Keep weak models out of chains used for judgment.** The quality floor is
  enforced by *omission*: reliability scoring can reorder a chain but can never
  introduce a model that is not listed in it, so a chain whose every member is
  above your bar stays above it.

`/v1/embeddings` is refused by default (**402**): OpenRouter publishes no free
embedding model, so that endpoint spends real credit and is the one path with no
chain, breaker or fallback behind it. Opt in with `PROXY_ALLOW_PAID_EMBEDDINGS=true`.

### Model chains (edit without recompiling)

The per-profile model chains live in **`model-policy.json`** — the live,
editable source. Change it and restart; no rebuild needed. Free OpenRouter
slugs get retired periodically, so this is the file you touch when a model
starts 404-ing (and the fallback chain already advances past a retired model
to the next one automatically).

Load order (later wins):

1. Embedded default (`internal/router/config/model-policy.default.json`) — a
   last-resort baseline so the binary always boots with a valid policy.
2. `model-policy.json` — looked up at `PROXY_MODEL_POLICY_FILE`, then next to
   the executable, then the working directory.
3. Env overrides (`PROXY_PROVIDER_PROFILES_JSON`, `PROXY_DEFAULT_PROFILE`, …)
   for one-off/ephemeral changes.

Refresh the free-model list from OpenRouter:

```bash
curl -s https://openrouter.ai/api/v1/models -H "Authorization: Bearer $OPENROUTER_API_KEY" \
  | jq -r '.data[] | select(.id|endswith(":free")) | select((.pricing.prompt|tonumber)==0) | .id'
```

### Updates (self-update + notice)

CalvoProxy knows its own version and checks GitHub Releases for a newer one.

- **On startup** (versioned builds only) it does one best-effort, non-blocking
  check and, if a newer release exists, logs a recommendation — `run
  calvoproxy update` for a binary install, or a `docker pull` line when it
  detects it's running in a container. Disable with `PROXY_UPDATE_CHECK=false`.
- **`GET /version`** reports the running build and the cached check result:
  `{"version":"v0.2.2","latest":"v0.2.3","update_available":true,"checked":true}`.
- **`calvoproxy update`** (binary installs) upgrades in place: it downloads the
  release archive for your OS/arch, **verifies its SHA-256** against the
  release's `SHA256SUMS.txt`, extracts the binary and swaps it atomically
  (on Windows the old exe is moved aside to `calvoproxy.exe.old` and cleaned up
  on next start). Restart afterwards to run the new version. Verification is
  **fail-closed**: if a release has no `SHA256SUMS.txt` (or no matching entry)
  the update is refused — pass `--insecure` to override (unsafe; only skips the
  checksum, not HTTPS). `--force` re-installs even when already current.
  `calvoproxy version` just prints the version.

  ```bash
  calvoproxy update
  ```

  **Signature verification is ON in official builds.** On top of the SHA-256
  check, the updater verifies an **Ed25519 signature** over `SHA256SUMS.txt`,
  which authenticates a release against a compromised host/account (a bare
  checksum cannot). The release public key ships in
  [`internal/releasekey`](internal/releasekey/key.go), so `calvoproxy update` is
  **fail-closed on the signature**: a missing or invalid `SHA256SUMS.txt.sig`
  refuses the update, and `--insecure` cannot bypass it.

  Because of that, the release workflow **fails loudly** if the
  `RELEASE_SIGNING_KEY` secret is missing while a key is embedded (publishing
  unsigned would break every client's update), and it verifies the signature it
  just produced against that same embedded key before publishing.

  For a **fork**, set up your own signing once:

  1. Generate a keypair: `go run ./tools/gen`.
  2. Put the printed **public** key in
     [`internal/releasekey/key.go`](internal/releasekey/key.go) (safe to commit)
     — or ship it via `PROXY_UPDATE_PUBKEY` at runtime.
  3. Add the printed **private** key as the GitHub Actions secret
     `RELEASE_SIGNING_KEY` (repo → Settings → Secrets → Actions).

  Leaving the key empty disables signature verification (SHA-256 only); the
  workflow then allows unsigned releases without failing.

  Inside a container self-update is intentionally refused (an image is
  immutable) — pull a new tag and recreate instead:

  ```bash
  docker pull ghcr.io/cervantesh/calvoproxy:latest
  docker compose up -d   # or: docker rm -f calvoproxy && docker run … :latest
  ```

### Reliability of long streams

Streamed (`stream: true`) responses are **not** bounded by the per-request
timeout — a long-but-live completion is delivered in full. Instead a stream is
cut only if it *stalls*: no bytes for `PROXY_STREAM_IDLE_TIMEOUT` (default 120s),
with an absolute `PROXY_STREAM_MAX_DURATION` backstop (default 30m). Non-stream
requests get a per-attempt timeout plus an overall wall-clock budget across the
fallback chain, so a slow first model can't starve the fallbacks.

### gRPC transport

Alongside the HTTP API, CalvoProxy exposes a small gRPC `ProxyTransportService`
(unary `ChatCompletion` + `GetHealth`) on `GRPC_PORT` (default `9090`), backed by
the same routing engine. It is **unary only**: a request with `stream: true` is **rejected** with
`InvalidArgument` rather than silently buffering the whole stream in memory,
so use the HTTP API for token streaming. The proto lives at
`proto/calvoproxy/proxy/v1/transport.proto`; generated stubs are under
`gen/proto/proxyv1/`. When `PROXY_ADMIN_TOKEN` is set, `GetHealth` requires it via
gRPC metadata (`authorization: Bearer <token>`); `ChatCompletion` always needs an
API key. A bind failure on `GRPC_PORT` is non-fatal — the HTTP proxy keeps
serving. (Compose maps only `8080`; publish `9090` yourself if you need gRPC.)

### HTTP endpoints

- `GET /health` — service status, active policy hashes, configured profiles.
- `GET /version` — running build + whether a newer release is available.
- `POST /v1/chat/completions` — OpenAI-compatible chat completions.
- Per-profile routes: `/v1/{simple,coding,reasoning,agent,creative,vision}/chat/completions`.
- `POST /v1/messages` — Anthropic-compatible messages, routed through the same
  model chain / breaker / scoring / multi-model fallback as chat (targets the
  OpenRouter/Anthropic `/messages` shape; other providers don't expose it).
- `POST /v1/embeddings` — embeddings.

Quick check:

```bash
curl -s http://127.0.0.1:8080/health
```

## Sign in to OpenRouter (`calvoproxy login`)

Instead of copy-pasting an API key from the dashboard, authorize CalvoProxy via
OpenRouter's OAuth (PKCE) — handy for onboarding, since each user brings their own
revocable key:

```bash
calvoproxy login          # opens your browser → authorize → key stored locally
calvoproxy whoami         # show which key is configured (masked) and its source
calvoproxy logout         # remove the stored key
```

`login` runs a one-shot loopback callback server on an **unguessable path**
(32 random bytes), opens `https://openrouter.ai/auth`, and after you authorize,
exchanges the code for a **user-controlled** API key (verified via PKCE `S256`).

The secret path is what binds the callback to *your* login: another process on
the same machine can find the port but not the path, so it can neither inject its
own authorization code nor kill your login with junk — and unlike the OAuth
`state` parameter, that protection doesn't depend on the provider echoing
anything back. (It does not defend against a same-user attacker who can read the
auth URL out of your browser history or the browser's command line; the secret
travels in that URL, exactly as `state` would.) A mismatched `state` and an
unattributed `error=` are ignored rather than ending the login.

On top of that, a matching CSRF `state` is **required by default**
(`PROXY_OAUTH_REQUIRE_STATE`) — an interactive login confirmed OpenRouter echoes
it, so demanding it closes the login-CSRF hole outright instead of leaning on the
secret path alone. Set it to `false` only if your provider doesn't echo `state`;
the login then still works, protected by the secret path and PKCE. The key is written to
`<user-config-dir>/calvoproxy/openrouter.key` (`%AppData%` on Windows,
`~/.config` on Linux, `~/Library/Application Support` on macOS), `0600`.

- `--no-browser` prints the URL to open manually.
- `--key-stdin` stores a key piped in, no browser (`echo sk-or-v1-… | calvoproxy login --key-stdin`) — for headless/CI.

**Key precedence** for a keyless request is: request `Authorization` header →
`OPENROUTER_API_KEY` env → the stored login key. The stored key is **ambient**
like the env key, so on a public bind it is refused unless
`PROXY_ALLOW_ENV_KEY_PUBLIC=true` (a header key always wins and bypasses the gate).
For public/Docker deployments, inject `OPENROUTER_API_KEY` or pass a per-request
Bearer rather than relying on the login file.

## Install

### Docker (recommended)

Pull the published image and run it with your OpenRouter key:

```bash
docker run -d --name calvoproxy -p 8080:8080 \
  -e OPENROUTER_API_KEY=sk-or-v1-... \
  -v calvoproxy-scores:/data \
  ghcr.io/cervantesh/calvoproxy:latest
```

> **Why the volume.** The proxy learns which of its free models actually answer
> and reorders the chain accordingly. That is written to `/data/scores.json`;
> without a volume it is lost every time the container is recreated, and the
> chain re-pays the whole discovery cost on the next burst. Compose declares a
> named volume for you. Omit it only if you genuinely want a stateless
> container.

> **Exposing it safely.** The container binds `0.0.0.0`, so it is reachable by
> anything that can reach the port. It therefore does **not** spend its
> `OPENROUTER_API_KEY` for clients that send no key of their own — those get a
> `401`. Two knobs decide how open it is:
>
> - `-e PROXY_ADMIN_TOKEN=…` — gate `/health`, `/metrics` and `/admin/reload`
>   (otherwise they're world-readable).
> - `-e PROXY_ALLOW_ENV_KEY_PUBLIC=true` — let keyless clients spend the
>   container's key. Only do this when the port is reachable *only* by people
>   you'd hand the key to.
>
> Clients that send their own `Authorization: Bearer sk-or-…` always work and
> are unaffected.

Or with Compose (set `OPENROUTER_API_KEY` in your shell or a `.env` file):

```bash
docker compose up -d
```

To edit the free-model chains without rebuilding, mount your own file:

```bash
docker run -d -p 8080:8080 -e OPENROUTER_API_KEY=sk-or-v1-... \
  -v "$PWD/model-policy.json:/app/model-policy.json:ro" \
  ghcr.io/cervantesh/calvoproxy:latest
```

Build the image locally instead of pulling: `docker build -t calvoproxy .`

**Port already in use?** If `8080` is taken on the host (a local
`kube-apiserver`, another service, …), map a different host port — the proxy
still listens on `8080` inside the container:

```bash
docker run -d -p 18080:8080 -e OPENROUTER_API_KEY=sk-or-v1-... \
  ghcr.io/cervantesh/calvoproxy:latest
# then use http://localhost:18080
```

**`docker pull` says `denied` even though the image is public?** Your Docker is
holding stale `ghcr.io` credentials. Clear them and pull anonymously:

```bash
docker logout ghcr.io
docker pull ghcr.io/cervantesh/calvoproxy:latest
```

### Prebuilt binaries (Windows / macOS / Linux)

Download the archive for your platform from the
[Releases](https://github.com/cervantesh/calvoproxy/releases) page. Each archive
(`calvoproxy-<version>-<os>-<arch>.zip`/`.tar.gz`) contains:

- `calvoproxy` (or `calvoproxy.exe` on Windows) — the server, a single static binary;
- `model-policy.json` — the editable free-model chains (optional; the binary has
  an embedded default, so it runs without this file);
- `README.md`, `LICENSE`.

**Windows** (from the extracted folder):

```powershell
$env:OPENROUTER_API_KEY = "sk-or-v1-..."
.\calvoproxy.exe
```

**macOS / Linux:**

```bash
export OPENROUTER_API_KEY=sk-or-v1-...
./calvoproxy
```

On macOS the binary is unsigned, so the first run may need
`xattr -d com.apple.quarantine ./calvoproxy` (or approve it in System Settings →
Privacy & Security). The proxy then listens on `http://localhost:8080` — point
any OpenAI-compatible client at it. Get a free OpenRouter key at
<https://openrouter.ai/keys>.

## Claude Code plugin

This repo also ships a Claude Code plugin so Claude Code (not just Hermes) can
query the free models through the proxy — for second opinions, cheap subtasks,
and drafts. It's under [`plugins/calvoproxy/`](plugins/calvoproxy/) and this repo
doubles as a plugin marketplace:

```text
/plugin marketplace add cervantesh/calvoproxy
/plugin install calvoproxy@calvoproxy
```

Then use `/ask-free <prompt>` or ask naturally ("second opinion from the free
model"). Minimal config: a reachable proxy (or set `CALVOPROXY_BIN` +
`OPENROUTER_API_KEY` to let the plugin start it on-demand). See the
[plugin README](plugins/calvoproxy/README.md).

## Layout

- `cmd/` — server entrypoint (HTTP wiring, idle shutdown, metrics).
- `internal/router/` — request classification, policy evaluation, model
  attempts, retries, circuit breaker.
- `internal/telemetry/` — OpenTelemetry setup.
- `docs/POLICY.md` — CervoRules v3 / CervoModelPolicy integration and runtime
  overrides.
- `test/contract/` — opt-in tests against the **real** OpenRouter API. Mocks
  encode assumptions; these check the assumptions. Run with
  `CALVOPROXY_CONTRACT=1 OPENROUTER_API_KEY=... go test ./test/contract/ -v`.
- `vendor/` — vendored dependencies (do not edit by hand).

## Changes and dependencies

- [CHANGELOG.md](CHANGELOG.md) — every release, what it changed, and the failure
  it came from. Retractions stay in the record.
- [THIRD_PARTY.md](THIRD_PARTY.md) — the `cervo-*` modules have **no `go.sum`
  entries and no reachable upstream**. Read this before trusting, auditing, or
  forking the build.

CI enforces three ratchets beyond the test suite: a coverage floor
(`scripts/coverage-gate.sh`), a checksum manifest over the vendored modules
(`scripts/vendor-manifest.sh`), and — when the repository secret is present —
the upstream contract tests.
