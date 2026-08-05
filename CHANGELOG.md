# Changelog

All notable changes to CalvoProxy. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); versions follow
[Semantic Versioning](https://semver.org/).

Every entry says **what changed and why it mattered**, because most of these
releases came out of a specific failure that a user actually hit. Where a
release was wrong, the retraction stays in the record rather than being edited
out — see v0.7.1.

## [Unreleased]

### Added
- **The response now says *why* this model answered, not only which one.**
  `X-Calvoproxy-Model` has told callers which model served since early on,
  because a chain that degrades silently caused a real incident — a design
  review answered by the third model and believed to be the first. It still
  could not answer the next question anyone asks: *why that one?* Which models
  were skipped for an open circuit, which failed first and with what code, what
  score the chain was ordered by. That lived only in a log line the caller never
  sees.

  A new `X-Calvoproxy-Route` carries it in one compact, versioned field, capped
  at 512 bytes:

  ```
  v1;p=coding;s=0.83;a=2;n=4/4/3;prev=gpt-oss-20b:429,gemma-4-31b:500;brk=1;cmp=off
  ```

  It reads: profile `coding`, served with score 0.83 on the second attempt, of
  four planned models three were eligible, one excluded by the breaker, and two
  failed first — one rate-limited, one with a server error. `cmp=` is always
  present so "not compressed" never looks like a missing field.

  The four exits that never reach a model — pinned model lacking a capability,
  no capable model anywhere, everything cooling down, chain exhausted — emit the
  same header with `o=<outcome>`. An HTTP 503 cannot distinguish "everything is
  in cooldown" from "nothing here can do vision"; now it does not have to.

  `prev=` reports the **upstream's** status, not the proxy's remapped one: the
  retry classifier turns a 500 into a 502, and a trace that reported 502 would
  mislead precisely the person trying to find out what OpenRouter said.

  Only status codes and a closed set of reason words travel in the header —
  never upstream error text. Streaming included: the header commits before the
  first byte, so a streamed answer explains itself too. gRPC inherits it for
  free. `PROXY_ROUTE_TRACE=off` removes the whole thing, and off means no trace
  is allocated at all, not a blanked header.

- **`GET /decisions/{id}` for the detail the header has no room for.** Every
  response now carries `X-Calvoproxy-Decision-Id`, and the last 200 decisions
  (`PROXY_TRACE_RING`) are kept in memory for lookup: the upstream error text
  behind each failed attempt, and the stream outcome, which is only known after
  the headers are already on the wire.

  Admin-gated, like `/health`, because that error text is the one part of a
  trace that comes from outside. A client that wants structure rather than the
  compact form can ask for it per request with `X-Calvoproxy-Trace: full` and
  gets the same JSON *without* the upstream text — that channel has no gate in
  front of it. The ring is never written to disk: these records sit next to
  conversation content, and `/metrics` remains the durable series.

### Fixed
- **A header the upstream echoed could appear twice.** `streamProxyResponse`
  copies upstream headers with `Add`, and it runs *after* the proxy sets its
  own, so an upstream emitting `X-Calvoproxy-Route` would leave the client with
  two values — the second one upstream-controlled text presented as this proxy's
  routing decision. The trace headers are now excluded from that copy.

  `X-Calvoproxy-Model`, `-Profile` and `-Attempt` have the same exposure and are
  not covered by this change.

## [0.10.1] — 2026-08-04

### Fixed
- **Score persistence now actually works in a container.** 0.9.2 made learned
  reliability scores survive a restart, and that fix stopped at the host: the
  image never set `PROXY_SCORE_FILE` and Compose mounted no volume, so a
  container kept relearning which models work on every recreation — the exact
  bug 0.9.2 fixed, still standing where it is most likely to bite. Compose sets
  `PROXY_IDLE_TIMEOUT` and `restart: on-failure`, so recreation is the normal
  cycle there, not a rare event.

  The missing volume was the silent half: nothing errors, the proxy serves
  normally, and the scores are simply gone after `docker compose down && up`.
  `docker-compose.yml` now mounts a named `calvoproxy-scores` volume over
  `/data`, and the image pins `PROXY_SCORE_FILE=/data/scores.json`.

  The pinned path is not only tidiness. Under `--user`, the default resolves to
  `/.config` (Docker sets `HOME=/` for a UID absent from `/etc/passwd`) and
  every flush fails with `mkdir /.config: permission denied`. And `/data` is
  created `1777`, not `755`: a mounted volume inherits the image directory's
  bits, so a root-owned `755` broke `--user` with
  `open /data/.scores-*.tmp: permission denied`. Both were measured against a
  running container, not reasoned about — the first draft of this fix claimed
  the default went *pathless* without `$HOME`, which testing disproved: Docker
  always defines it.

  Guarded by tests asserting the Dockerfile and Compose agree, because the
  headline failure mode is silence: there is no behaviour to observe when the
  volume goes missing, only a proxy that performs worse than it should.

### Changed
- `.gitignore` now covers `*.exe.*`. The existing `*.exe` did not match a
  *renamed* binary — `calvoproxy.exe.old` (left by `calvoproxy update`) or
  `calvoproxy.exe.bak-pre-v0.7.0` — which is how 21 MB of stale binary got
  committed in #32 and had to be removed again in #39.

## [0.10.0] — 2026-08-04

### Added
- **Failed requests now say why.** Measured on a live instance (v0.9.1, 183
  requests, 2.8h uptime): 26 requests — 14% — returned 5xx and nothing recorded
  a cause. `/metrics` could say a request failed and could say which circuits
  were open, but never why a fallback chain gave up.

  New `calvoproxy_chain_failed_total{reason}` with five causes: `cancelled`
  (the client hung up), `total_timeout` (the whole-chain budget expired),
  `terminal` (a non-retryable error stopped the chain **with models still
  untried**), `exhausted` (every model tried, every one failed), and
  `executor_error` (misconfiguration). Counted once per failed chain at a single
  site, from the chain's own verdict plus the request context.

  The reason cannot be inferred from how the loop exited, which is the trap this
  went through two designs to avoid. `executeAttempt` turns a cancelled parent
  context into `SkipModel`, and the fallback loop treats `SkipModel` as
  *continue* — so a client hanging up does **not** stop the chain. It burns every
  remaining model one cancelled attempt at a time and leaves through the normal
  end of the loop, where the exit path reads `exhausted` and the error's shape
  (`Retryable:false`) reads `terminal`. Both would blame the models for the
  client. Only `ctx.Err()` separates them, so it is checked first and wins.

  Likewise `terminal` is not "did we break": a non-retryable error on the *last*
  model breaks with nothing left untried, which is diagnostically `exhausted`.
  What makes `terminal` worth alerting on is that options remained unspent.

  **Known limit, stated rather than left to be discovered:** `total_timeout`
  will read at or near zero on a streaming-heavy instance. The whole-chain
  budget is only applied to non-streaming requests, and 139 of the 183 measured
  requests were streams. A zero there is not evidence that chains never time
  out.

- **`calvoproxy_all_models_cooling_total`** — the client-visible 503 that had no
  instrumentation at all. When every planned model is circuit-open or cooling
  down, the request is refused *before* the fallback executor runs, so it never
  reached any chain-level counter, even though its neighbours (admission
  rejections, capability refusals) were both counted. Kept deliberately separate
  from `chain_failed`: the chain never ran, and folding it in would report a
  failure for attempts that were never made.

- **Two per-model latency series, which must never be summed together.**

  `calvoproxy_attempt_first_event_seconds_{sum,count}{model}` is the wait
  **after** response headers — exactly the quantity
  `PROXY_STREAM_FIRST_BYTE_TIMEOUT` compares against, so it is the series, and
  the only one, that says whether that budget is tuned correctly. Recorded on
  **both** outcomes: if only timeouts were sampled every value would equal the
  budget, and the healthy population that decides the tuning question would
  never appear.

  `calvoproxy_attempt_first_token_seconds_{sum,count}{model}` is the whole
  request → first token, clock started before the upstream call so it includes
  the wait for headers. This is the series that ranks models by responsiveness.
  Recorded only when a token actually arrived — folding in "how long we waited
  before giving up" would drag a model's number toward the budget and make it
  look slower the more often it was abandoned, which is the backwards-ranking
  bug this design set out to avoid, in a new place. Abandonments stay counted by
  `calvoproxy_stream_first_event_timeout_total`.

  The second series exists because the first one, run against the real upstream,
  under-measured badly: a request with a **1.51s** time-to-first-token recorded
  **under 5ms** of post-header wait. OpenRouter held the headers and then
  flushed its `: OPENROUTER PROCESSING` keepalives and the first data event
  together, so nearly all the delay landed before the headers did — invisible to
  a stopwatch that starts when `Client.Do` returns. Across 12 further live
  requests the recorded post-header waits were 0.00-0.67s while measured
  time-to-first-token was 0.42-2.24s. The post-header wait was measuring what
  the budget acts on, correctly, and answering the operator's actual question
  ("which model is slowest") wrongly.

  Deliberately *not* measured at `Client.Do` returning, despite that being the
  obvious "comparable" choice. Those are two different quantities: for a
  non-streaming attempt the upstream headers arrive when generation is
  essentially finished (tens of seconds), for a streaming one they only mean
  "accepted" (~0.5s) — averaging both per model produces a meaningless number.
  Worse, it ranks backwards for the exact failure worth catching: the
  abandonments happen *after* `Do` returns, so a model that accepts instantly
  and then queues would record a fast sample while being abandoned for slowness.

  Both are labelled with the same `profile:model` key as
  `calvoproxy_model_score`, so they join and cardinality stays bounded by the
  policy. Both are sampled only where the first-event wait actually runs —
  streaming attempts, with `PROXY_STREAM_FIRST_BYTE_TIMEOUT` set, that are not
  last in the chain — so neither count is the model's request count.

  Context for reading it: the same live instance abandoned 53 of 183 requests
  (29%) at the first-event budget, with `PROXY_STREAM_FIRST_BYTE_TIMEOUT=3`.
  Healthy time-to-first-event is 0.35-0.70s with a slow tail of 9-13s on the
  *same model and prompt* — variance, not a broken model. A 3s budget abandons
  that whole tail, so 29% is at least as consistent with a budget tuned too
  tight as with bad models. This histogram is what decides which; the knob is
  free to change. No behaviour was changed here.

`calvoproxy_requests_by_status` and `calvoproxy_request_latency_*` are
unchanged. The latter still answers "what did the user wait", which is a
different and still-valid question from per-model first-token latency.

## [0.9.2] — 2026-08-03

### Fixed
- **Per-model reliability scoring was inert in production; it now works.**
  Measured on a live instance on 2026-08-03 (v0.9.1, 183 requests of real
  traffic), every model in `/metrics` reported exactly `calvoproxy_model_score
  0.8000` — a model with 96 successes scoring identically to one with 0
  successes and a standing consecutive failure. The subsystem that exists to
  reorder the chain was reordering nothing.

  Two independent causes, both fixed:

  - **Decay was measured in wall-clock time over a five-minute window.** Every
    score drifted linearly to the neutral baseline (0.8) after five idle
    minutes, and `rankAttemptsByScore` stable-sorts — so with all scores equal
    it reproduced the policy-defined order verbatim. Five minutes suits a
    service under constant load; this workload is bursty and interactive (used
    for a while, idle for 30+ minutes, used again), so the memory evaporated
    precisely during the gaps, when nothing had actually changed about the
    models. Decay is now measured against **two** clocks and advances at the
    slower of them: elapsed time (`PROXY_SCORING_RECOVERY_SECONDS`, default
    6 h — hours, not minutes) and the proxy-wide count of scored attempts
    (`PROXY_SCORING_RECOVERY_ATTEMPTS`, default 50). A model nobody has retried
    has produced no new evidence either way, so its score no longer moves.
  - **The score map was in-memory only.** `modelBreakers` was created fresh in
    `NewRouterService` and nothing persisted it, so every restart discarded all
    learned reliability — installing v0.9.1 on 2026-08-03 wiped it. Scores now
    persist to `PROXY_SCORE_FILE` (default
    `<user-config-dir>/calvoproxy/scores.json`, `off` to disable), flushed every
    30 s when dirty and once more on clean shutdown, reloaded at startup.
    Deliberately narrow: breaker state (`OpenUntil`, `ProbeUntil`,
    `ConsecutiveFailures`) is **not** persisted, because a cooldown is a
    statement about right now and a restart is a good reason to re-probe. Files
    and entries older than `PROXY_SCORE_MAX_AGE_SECONDS` (default 24 h) are
    discarded, and keys the current policy no longer names are dropped on load.

  Why it mattered: 53 of 183 requests (29%) hit `stream_first_event_timeout` —
  the first-chosen model was too slow to emit an event and the chain advanced —
  at an average handler latency of 14.9 s. The chain learned which model worked,
  forgot within five minutes, and re-paid the abandonment cost on the next
  burst.

### Changed
- **A model that has never once succeeded is now treated differently from one
  that had a bad day.** Zero successes is not the same evidence as a bad spell:
  a bad spell is contradicted by the successes around it, while "0 successes,
  N failures" has no counter-evidence at all. Such a model now drifts back to a
  lower baseline (0.5) instead of the neutral 0.8, so it ranks below a model
  that actually recovered. It is still tried — last — and one real success moves
  it onto the normal baseline for good. This is the case the live instance
  showed directly: gemma-4-31b, 0 successes and a standing failure, scoring
  identically to everything else.

## [0.9.1] — 2026-08-03

### Fixed
- **A provider's failure no longer ends the whole chain, whatever its status.**
  On 2026-08-03 a request died on `401 authentication_error: "invalid API key"`
  — from a *provider*, relayed by OpenRouter, while the account's own key was
  valid and answering `/api/v1/key` with 200. 401 is terminal, so the chain
  stopped on a provider-side credential problem the next model would not have
  hit; a direct request to the same profile succeeded first try on a different
  provider minutes later.

  This is the 64-tool incident again with a different number. Both bodies have
  the same shape — `"message":"Provider returned error"` plus a
  `metadata.provider_name` — and both came from the same provider. So the rule
  is no longer "which status codes advance" but **who refused**: a relayed
  provider failure advances, whatever the code, and a genuine account-level
  rejection (no `provider_name`) still terminates, because a bad key is bad for
  every model and burning the chain would bury the one error worth reading.

  Recorded as unchanged in 0.9.0, 402 is now covered by the same rule.

## [0.9.0] — 2026-08-01

Nothing in this release changes how the proxy routes a request. It changes what
the project can prove about itself, and it raises the Go floor.

**Minor, not patch, for one reason: this requires Go 1.26 to build.** Anyone on
1.25 will fail, which is not a patch-level surprise. There is no behavioural
change to the running proxy.

### Added
- Contract tests against the **real** OpenRouter API (`test/contract/`,
  opt-in via `CALVOPROXY_CONTRACT=1`). Every mock in this repo encodes an
  assumption about the upstream; when one is wrong the suite stays green and
  production breaks. That is exactly how the 64-tool cap shipped. These pin the
  assumptions the router bets on: the tool cap surfaces as **400** (load-bearing —
  the chain only advances past 400), the chain leader accepts the payload size
  real agents send, the stream is SSE with `data:` events and `[DONE]`, and every
  model in `model-policy.json` still exists and declares its capabilities.
- Critical-path test coverage: `RouteRequestWithProvider` 41.7% → 87.5%,
  `dispatchChain` 45.2% → 92.9%, `streamProxyResponse` 0% → 100%.
- `TestUpstreamStatus_AdvancesOrTerminatesTheChain` — a table over upstream
  status classes asserting **advance vs terminate**. This is the actual guard
  for the 64-tool incident: mutation-checked, removing `SkipModel` on 400 fails
  it. "Which statuses advance" is the question that keeps producing incidents,
  and answering it one status at a time is how the 400 case was missed.
- Coverage ratchet (`scripts/coverage-gate.sh` + `scripts/coverage-floors.txt`),
  wired into CI. Floors only move up: `--update` refuses to lower without
  `--allow-lower` or to drop an entry without `--allow-remove`, because
  regenerating a ratchet in the same PR is the easy way to defeat it without
  ever arguing with it.

  Function floors are pinned to one decimal and bind on every platform; package
  totals are pinned to whole percent and bind only on CI. That asymmetry is
  measured, not stylistic: across three consecutive CI runs on the same code
  `internal/router` reported 81.8, 81.7 and 81.6 — one of those runs changed
  nothing but the floors file — while every pinned function measured identically
  in all three runs and on both Windows and Linux. A gate that fails on
  scheduling noise gets switched off, and then it protects nothing.
- `CHANGELOG.md` (this file) and a release checklist, so a release is a
  reviewable act rather than a tag with narrative attached.

### Changed
- After external adversarial review, several of the guards above were found to
  be weaker than they read. Fixed: the degraded-attempt assertion now requires
  exactly `"2"` (it accepted any non-empty value, so a signal hard-wired to
  `"1"` would have passed); `Retry-After` must parse to a positive value in a
  usable band, not merely exist; the embeddings opt-in asserts the client's
  status, not just that money was spent. `--update` on the coverage gate can now
  only *raise* floors — lowering needs `--allow-lower`, removing needs
  `--allow-remove` — because regenerating a ratchet in the same PR is the easy
  way to defeat it. The float tolerance dropped from 0.05 to 0.001, which was
  large enough to hide real drops. The vendor manifest hashes every regular
  file rather than three extensions, closing a hole that would have opened the
  day a vendored module gained a `//go:embed` asset. The tool-cap contract test
  now fails loudly instead of skipping when the cap disappears.
- Recorded, not changed: **402 terminates the chain**, found by the new
  status-class table. It looks like the same bug class as the 400 — a free model
  that starts requiring credit kills a turn the rest of the chain would have
  served — but changing routing semantics does not belong in a testing change.
  See `TestUpstreamStatus_AdvancesOrTerminatesTheChain`.

### Changed
- **Go 1.25.8 → 1.26.5** across `go.mod`, both CI workflows and the Dockerfile.
  Statement counting differs slightly between toolchains, so one package's
  coverage floor moved by 0.1pp; no critical-path function changed. That is the
  case `--allow-lower` exists for, and it is now documented in the gate.
- Every GitHub Action bumped to its current major: `checkout` v4→v7, `setup-go`
  v5→v7 (both were pinned to a deprecated Node 20 runtime), `action-gh-release`
  v2→v3, `setup-buildx` v3→v4, `login-action` v3→v4, `metadata-action` v5→v6,
  `build-push-action` v6→v7. Every input this repo passes was checked against
  each new major's `action.yml` first — the release workflow signs binaries and
  publishes images, and it cannot be tested without cutting a release.

### Fixed
- The vendor manifest hashed bytes on disk, so with `core.autocrlf=true` (the
  Windows default) all 54 files read as modified on a Linux CI runner. It now
  hashes the content **git** holds, which is identical on every platform and
  every git config, and `.gitattributes` keeps `vendor/**` checked out
  byte-exact besides.
- `TestHostBreaker_OpensOn5xxAndSingleFlightsRecovery` was flaky and failed a
  CI run with “got 2” — where **both probes were legitimate**. The probe's own
  503 re-opens the circuit for another cooldown, so any caller the scheduler
  delays past that second window enters a second half-open window and correctly
  sends a second probe; under `-race` on a loaded runner, starting 16 goroutines
  easily outlasts a 50ms cooldown. The test no longer sleeps: it forces the
  half-open state directly and uses a cooldown long enough that no second window
  can open, so it measures the single-flight invariant rather than the
  scheduler. 60/60 under `-race`, and breaking the single-flight makes it fail
  with 15 probes. (v0.5.2 made the *model* breaker test deterministic; the
  *host* breaker one was still timing-dependent.)
- CI's contract-test guard used the `secrets` context in a step-level `if:`,
  which is a workflow file error — two runs failed in 0s with no log. The env
  has to be declared at **job** level; a step's own `env:` block is invisible to
  that step's `if:`. The tests now run wherever a key exists and are skipped
  cleanly wherever one does not — which is most places, since forks cannot read
  secrets and a contributor's clone has no key at all. A skipped run says so in
  the job summary: absence is visible, never fatal. Requiring the key would
  paint every contributor's CI red for a reason unrelated to their change, which
  is a good way to teach people to ignore CI. This is a public project, not one
  deployment's private pipeline.

## [0.8.0] — 2026-08-01
### Added
- `X-Calvoproxy-Model`, `X-Calvoproxy-Profile` and `X-Calvoproxy-Attempt` on
  every response, streaming included. A profile name is a request, not a
  promise: the chain reorders by reliability and falls through on failure, so
  `coding` can be served by any member. `Attempt > 1` is the degraded signal.
  It exists because silent degradation caused a real incident — a design review
  answered by the third model in the chain, believed to be the first.

## [0.7.2] — 2026-08-01
### Fixed
- A single provider's `400` no longer ends the whole chain. One provider
  rejected requests carrying more than 64 tools
  (`at most 64 tools are allowed`); 400 was terminal, so every agent turn that
  reached it died in ~0.8s even though every other model in the chain would
  have answered. 400 now advances to the next model.

## [0.7.1] — 2026-08-01
### Fixed
- Unknown-profile rejection is opt-in (`PROXY_STRICT_PROFILE_NAMES`), so a
  caller's own model names are no longer 404'd by default.

> **Retraction.** The original notes for this release attributed an ongoing
> Discord failure to this gate, based on a bot reply claiming it was "using the
> model Sol". That was model output, not telemetry — `Sol` appears nowhere in
> the configuration, and the routing log showed `category=coding` with zero
> profile rejections. The real cause was the 64-tool 400, fixed in v0.7.2. The
> published notes were edited to carry this correction.

## [0.7.0] — 2026-08-01
### Added
- Fail-fast on a queued model: if no stream event arrives within
  `PROXY_STREAM_FIRST_BYTE_TIMEOUT`, abandon it **before** committing headers
  and try the next model. Measured healthy time-to-first-event is 0.35–0.70s
  while the slow tail ran 9–13s on the *same* model and prompt. Only `data:`
  lines count as progress — keepalive comments arrive precisely while a request
  is queued, so counting them would defeat the check in the one case it exists
  for. Skipped on the last attempt, where abandoning converts a slow success
  into a fast failure.

## [0.6.1] — 2026-08-01
### Fixed
- A cancelled request no longer blames the model or the host. A client hangup
  was being scored, counted toward the model's circuit breaker, and counted
  against the shared host breaker — so a few disconnects in a row could open
  `openrouter.ai` for *every* model. `DeadlineExceeded` is deliberately still
  counted: that is our own timeout firing, which is real evidence.

## [0.6.0] — 2026-08-01
### Added
- Verifiable capability floor for the agent and review chains. Scoring can
  reorder a chain but can never add a model to it, so the floor is enforced by
  omission: a chain that must not degrade simply does not contain a weak model.

## [0.5.2] — 2026-08-01
### Fixed
- Deterministic half-open breaker test (it was flaky, and a flaky check trains
  people to merge past red).

## [0.5.1] — 2026-08-01
### Fixed
- Agent image requests use the curated vision chain even when tools are
  present. An unknown model counts as usable here: curated configuration beats
  missing index data.

## [0.5.0] — 2026-08-01
### Added
- Fail-closed `critic` profile. Profiles differ by *the failure you can
  tolerate*, not by task name: `critic` would rather refuse than answer from a
  weak model, because a wrong review is worse than no review.

## [0.4.3] — 2026-08-01
### Added
- `calvoproxy doctor` — first-run self-check. Written after getting a working
  Hermes↔proxy setup took hours: it checks the things that actually went wrong
  (BOM in `config.yaml`, base-URL normalisation matching the client's, provider
  binding, reachability).

## [0.4.2] — 2026-07-31
### Security
- CSRF `state` required by default on the OAuth callback (verified against
  OpenRouter).

## [0.4.1] — 2026-07-31
### Security
- OAuth callback hardening: secret callback path, opt-in strict state.

## [0.4.0] — 2026-07-31
### Fixed
- Honest stream scoring: a stream that dies mid-response is no longer recorded
  as a success.
- Host-breaker neutrality on 429.
### Security
- Release-signing safety; secure Docker defaults.

## [0.3.1] — 2026-07-31
### Added
- Wider vision chain (4 free vision models).

## [0.3.0] — 2026-07-31
### Added
- OpenRouter OAuth PKCE login (`calvoproxy login`).

## [0.2.9] — 2026-07-31
### Added
- Capability-aware routing (vision + tools), fail-closed: a model with no
  declared capability is never selected for a request that needs one.

## [0.2.8] — 2026-07-31
### Security
- First signed release; self-update signature verification active.

## [0.2.7] — 2026-07-31
### Added
- Self-update signatures; `/messages` routed through the model chain.

## [0.2.6] — 2026-07-31
### Added
- Load-test CI gate, admission control (`PROXY_MAX_CONCURRENT`), `Retry-After`
  on cooldown responses.
### Fixed
- Scoring race.

## [0.2.5] — 2026-07-31
### Fixed
- Upstream connection pooling — a stress-test finding, 6.5× throughput.

## [0.2.4] — 2026-07-31
### Security
- Resilience/reliability/security hardening from a three-engine review.

## [0.2.3] — 2026-07-30
### Added
- Self-update and update detection (binary and Docker).

## [0.2.2] — 2026-07-30
### Fixed
- Restored gRPC transport under a neutral proto namespace.

## [0.2.1] — 2026-07-30
### Added
- Optional `/health` + `/metrics` auth; hot-reload of `model-policy.json`.

## [0.2.0] — 2026-07-30
### Changed
- Resilience/reliability/DX hardening; completed the project rename to
  CalvoProxy.

## [0.1.2] — 2026-07-30
### Added
- Per-model reliability scoring that reorders the chain, so flaky models sink
  to the back.

## [0.1.1] — 2026-07-30
### Fixed
- Idle-shutdown robust to stray pollers; gRPC bind failure is non-fatal.

## [0.1.0] — 2026-07-30
### Added
- First public release: open-source scaffolding, Docker, CI/release pipeline.

[Unreleased]: https://github.com/cervantesh/calvoproxy/compare/v0.10.1...HEAD
[0.10.1]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.10.1
[0.10.0]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.10.0
[0.9.2]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.9.2
[0.9.1]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.9.1
[0.9.0]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.9.0
[0.8.0]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.8.0
[0.7.2]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.7.2
[0.7.1]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.7.1
[0.7.0]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.7.0
[0.6.1]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.6.1
[0.6.0]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.6.0
[0.5.2]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.5.2
[0.5.1]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.5.1
[0.5.0]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.5.0
[0.4.3]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.4.3
[0.4.2]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.4.2
[0.4.1]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.4.1
[0.4.0]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.4.0
[0.3.1]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.3.1
[0.3.0]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.3.0
[0.2.9]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.2.9
[0.2.8]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.2.8
[0.2.7]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.2.7
[0.2.6]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.2.6
[0.2.5]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.2.5
[0.2.4]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.2.4
[0.2.3]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.2.3
[0.2.2]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.2.2
[0.2.1]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.2.1
[0.2.0]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.2.0
[0.1.2]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.1.2
[0.1.1]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.1.1
[0.1.0]: https://github.com/cervantesh/calvoproxy/releases/tag/v0.1.0
