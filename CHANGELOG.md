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
  wired into CI. Floors sit at the last measured value and only move up, so a
  hurried PR cannot quietly trade coverage for speed. Lowering one is a visible
  diff line.
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

### Fixed
- The vendor manifest hashed bytes on disk, so with `core.autocrlf=true` (the
  Windows default) all 54 files read as modified on a Linux CI runner. It now
  hashes the content **git** holds, which is identical on every platform and
  every git config, and `.gitattributes` keeps `vendor/**` checked out
  byte-exact besides.
- CI's contract-test guard used the `secrets` context in a step-level `if:`,
  which is a workflow file error — two runs failed in 0s with no log. The env
  has to be declared at **job** level; a step's own `env:` block is invisible to
  that step's `if:`. A missing key is now a loud failure on pushes to main and
  dev, rather than a step that skips forever and reads as a pass.

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
- Resilience/reliability/DX hardening; completed the CervoProxy → CalvoProxy
  rename.

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

[Unreleased]: https://github.com/cervantesh/calvoproxy/compare/v0.8.0...HEAD
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
