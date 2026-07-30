# CalvoProxy

Smart OpenAI-compatible proxy that fronts multiple LLM providers behind one
endpoint. It applies deterministic request policy (CervoRules v3), selects a
model chain per request (CervoModelPolicy), and adds gateway concerns —
timeouts, retries, circuit breaking, limits and audit — on top of upstream
forwarding.

This repository is a **standalone extraction of CalvoProxy** from the
`cervoclaw` monorepo. All of its dependencies are **vendored** (`vendor/`), so
it builds and runs on its own with no access to the monorepo or to the private
Gitea modules it originally depended on.

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

The server exposes an HTTP API and a gRPC API.

| Env var              | Default | Description                          |
|----------------------|---------|--------------------------------------|
| `PORT`               | `8080`  | HTTP listen port                     |
| `GRPC_PORT`          | `9090`  | gRPC listen port                     |
| `OPENROUTER_API_KEY` | —       | Upstream key for the default executor|
| `PROXY_IDLE_TIMEOUT` | off     | Exit after this idle period (Go duration, e.g. `20m`) — enables on-demand use |
| `PROXY_SCORING_ENABLED` | `true` | Reorder the chain by per-model reliability score (see below) |
| `PROXY_BREAKER_FAILURE_THRESHOLD` | `3` | Consecutive failures before a model's circuit opens |
| `PROXY_BREAKER_COOLDOWN_SECONDS` | `60` | How long an open circuit skips a model |

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
  removed, and **recovers toward neutral over ~5 min** so it gets retried later.
  Scores are visible under `circuits[].score` in `/health`. Set
  `PROXY_SCORING_ENABLED=false` to keep the static chain order.

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

### HTTP endpoints

- `GET /health` — service status, active policy hashes, configured profiles.
- `POST /v1/chat/completions` — OpenAI-compatible chat completions.
- Per-profile routes: `/v1/{simple,coding,reasoning,agent,creative,vision}/chat/completions`.
- `POST /v1/messages` — Anthropic-compatible messages.
- `POST /v1/embeddings` — embeddings.

Quick check:

```bash
curl -s http://127.0.0.1:8080/health
```

## Install

### Docker (recommended)

Pull the published image and run it with your OpenRouter key:

```bash
docker run -d --name calvoproxy -p 8080:8080 \
  -e OPENROUTER_API_KEY=sk-or-v1-... \
  ghcr.io/cervantesh/calvoproxy:latest
```

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

- `cmd/` — server entrypoint (HTTP + gRPC wiring).
- `internal/router/` — request classification, policy evaluation, model
  attempts, retries, circuit breaker.
- `internal/telemetry/` — OpenTelemetry setup.
- `docs/POLICY.md` — CervoRules v3 / CervoModelPolicy integration and runtime
  overrides.
- `vendor/` — vendored dependencies (do not edit by hand).
