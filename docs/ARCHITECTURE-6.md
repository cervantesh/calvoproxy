# CalvoProxy — High-level architecture for the six capabilities

Synthesis of a panel of three independent architects (two rounds: blind design, then
cross-arbitration), with every factual claim verified against the code.

Naming: **P1** decision header · **P2** quotas · **P3** compression · **P4** `setup-<tool>` ·
**P5** dashboard · **P6** `chat`.

---

## 0. The shape of the system in one sentence

A **single per-request trace structure**, owned by the request's goroutine and complete before
the first byte of the response, is the plumbing that P1, P5, P6 and the *measurement* of P2 and
P3 all hang from. **Quota (P2) does not live there**: it is durable state shared across
requests, a sibling of scoring, with its own file and its own key. P4 is tooling and does not
touch the router.

```
dispatchChain                     ← creates the trace, puts it in the ctx
  ├─ applyCapabilityFilter        ← annotates exclusions (capability)
  ├─ filterAvailableAttempts      ← annotates exclusions (breaker) + consults quotaLedger
  ├─ rankAttemptsByScore          ← annotates order + quota headroom factor (P2 soft)
  ├─ [P3] compression, ONCE       ← annotates compressionStats
  └─ executeFallbacks
       └─ Execute (loop)          ← annotates each attemptError
            └─ ExecuteAttempt
                 └─ executeAttempt
                      ├─ 429 → parseRetryAfter → recordFailure   ← P2 ingests consumption
                      └─ setServedModelHeaders                   ← P1 materialises here
```

---

## 1. Settled decisions

### D1 — The trace travels in the `context.Context`. Verified.

`setServedModelHeaders` is called at `router_upstream.go:209` (streaming) and `:259`
(non-streaming), both **inside** `executeAttempt` (which spans `:16`–`:263`).
`AttemptExecutor.ExecuteAttempt` (`router_types.go:157`) **does not receive
`FallbackExecution`**. So a field on that struct cannot reach the point where the trace is
materialised, nor the 429's `Retry-After` (`:138`), nor the first-token latency (`:204`), nor
the stream outcome (`:213-229`). The `ctx` reaches all of them.

The original reason for preferring the `ctx` ("adding a field breaks the signatures") is
**false**: the struct is passed by value, signatures name the type and not its fields, and the
nine literals in the repo (`router_fallback_test.go:42,73,103`;
`router_chain_failure_test.go:108,135,159,174,189`; `router.go:347`) all use keys. But the
conclusion survives for a better reason: **reach**, not compatibility.

**Decision:** a pointer in the `ctx` under a typed key, and *no* duplicating it as a field of
`FallbackExecution` as well — two sources for the same pointer is a smell, not readability. An
accessor `traceFrom(ctx)` returning `nil` out of band, with every `record*` a no-op on `nil`
(the pattern `s.capabilities != nil` already uses, `router.go:618`).

**Invariants to write in a comment and protect with `-race`:** a single writer (the request's
goroutine); `streamCopy` (`router_stream.go:97`) and `awaitFirstStreamEvent` (`:236`) do not
touch it; a compacted copy goes to P5's ring on close, and the live pointer is never published.

### D2 — Short header by default, full JSON on opt-in, `/decisions/{id}` as a third channel.

The versioned short form (`v1;p=coding;s=0.83;a=2;prev=…;caps=tools;cmp=-3.1k`, hard cap
512 B) is the stable contract. Full JSON only if the client sends `X-Calvoproxy-Trace: full`.
And `GET /decisions/{id}` over the ring, behind the same `admin` gate as `/health`.

The real reason is not size (in a single-hop loopback binary, 1–2 KiB breaks nothing, and gRPC
inherits it for free because `cmd/grpc.go:100` copies `recorder.Header()`): it is that the
trace carries other models' `LastFailureReason`, which is truncated upstream error body
(`truncateReason`, `router_breaker.go:559`). Emitting that by default on every response is
leakage, not verbosity. Always sanitise.

`cmp=` must be **always present**, even as `cmp=off`, so that "did not compress" stays
distinguishable from "the field does not exist".

### D3 — A separate `quotas.json`. Do not extend `scores.json`.

`LoadScores` discards the whole file if the version does not match
(`router_scoring_store.go:232`) or if it exceeds `maxAge` (`:237`), and `restoreScores` filters
by `knownBreakerKeys()` (`:133`). Putting quotas in there requires **three exceptions to those
three rules** in the same loader, plus a fourth problem: `snapshotScores` takes `breakerMu`
(`:94`), which would couple the quota flush to the breaker's lock. And the version bump
discards every score in the fleet, with v1 and v2 coexisting during rollout.

**What is shared:** extracting the atomic temp+rename helper from `writeScoreFile` (`:175`,
0600/0700) and the dirty-flag + 30 s flusher pattern into a common `statestore`.

**Its own lifecycle:** a quota's expiry is its `ResetAt`, not `defaultScoreMaxAge`. On load: if
`ResetAt` has passed, the window goes to zero; if not, `Used` is restored.

### D4 — Quota keyed by `model:<slug>` plus an `account` scope. Unanimous after arbitration.

`breakerKey` is `profile + ":" + model` (`router_breaker.go:189`), and that is **right for
reliability** — the same slug under `coding` and `agent` brings a different profile, a different
`decision.Timeout` (`router.go:344`) and a different body, so it deserves independent circuits
and scores.

It is **fatal for quota**: OpenRouter's allowance is per account and per model. With the same
slug across several profiles in `model-policy.json` (which is exactly the case today),
`coding:x` and `agent:x` would carry partial counters of the same pocket and the gate would
never fire in time. The `account` scope is mandatory from day one — it is the free tier's
dominant limit, and adding it later invalidates the persisted file.

### D5 — `quotaLedger` with its own state; reuse the *choke points*, not the state.

Writing `state.OpenUntil = windowReset` to inherit the breaker's filtering is contradicted by
three places in the code:

1. `recordSuccess` (`router_breaker.go:180`) and `resolveProbe` (`:160`) set
   `OpenUntil = time.Time{}` unconditionally. A request **in flight** that ends in a 200 would
   silently erase the quota exclusion just placed.
2. `recordFailure` resets `ConsecutiveFailures` and `OpenUntil` once the window expires
   (`:111-114`): a daily window would hold the half-open machinery hostage for 24 h.
3. `Health()` classifies any future `OpenUntil` as `"open"` and degrades `Status` (`:302` ff.):
   an exhausted quota would be reported as a broken circuit — precisely the confusing diagnosis
   P1 exists to eliminate.

**Design:** a ledger with its own mutex, consulted from `isModelAvailableLocked` (`:37`) and
`retryAfterForAttempts` (`:223`) *in addition to* the breaker — that way the 503's `Retry-After`
(`router.go:332`) is inherited just the same, taking the minimum of both.
**Mandatory lock order: `breakerMu → quotaMu`**, because `isModelAvailableLocked` is called from
`Health()` with `breakerMu` already held (`:340`); the ledger must not call anything that takes
`breakerMu`.

**Soft degradation by default:** a headroom factor ∈ [0,1] applied in `rankAttemptsByScore`
(`router_scoring.go:230`) **without touching the persisted score** — the score measures
reliability, not budget, and contaminating it poisons its two-clock decay. Hard exclusion only
under `PROXY_QUOTA_HARD_SKIP`.

**The breaker remains the reactive backstop.** Factual note: the 429 is neutral only in the
*host* breaker (`:509`), and that comment says it is neutral there *because the model breaker
does count it* — `executeAttempt:135-139` penalises hard and calls `recordFailure` with
`parseRetryAfter`. Quota is predictive; the breaker is reactive. Do not merge them.

### D6 — Two compression engines. Session dedup and prose pruning are out.

- **Tool-result truncate** — keep N bytes from each end with a `[truncated: X bytes]` marker,
  respecting code and JSON byte for byte. It is the highest-yield engine on agent workloads.
- **Intra-request dedup** — literal copies of the same block *within the history already being
  sent* are replaced by a reference to the copy that does travel. Deterministic by hash,
  self-contained, no state and no persistence.

**Cross-turn session dedup: discarded.** Against a stateless upstream, not resending the
context does not compress it — it makes the model stop seeing it. That is amnesia. An LRU with
a prefix hash does not save it either: the problem is not remembering the prefix, it is that it
has to be sent anyway.

**Semantic prose pruning: discarded for v1.** "Semantic + deterministic + no ML" is a practical
contradiction: preserving code "byte for byte" while pruning text requires delimiting code in
arbitrary Markdown, and one unclosed fence turns pruning into corruption. Only its non-semantic
form survives (collapsing whitespace and identical blocks), which dedup already covers.

**Hook:** a single pass in `dispatchChain` before `executeFallbacks` — never inside the loop,
which already re-serialises per attempt (`router_fallback.go:108`).
**Mandatory:** return a new map, because `execution.RequestBody["model"] = attempt.Model`
(`router_fallback.go:107`) mutates the shared map. Opt-in per profile in `model-policy.json`, a
`dry-run` mode (measures the saving, applies nothing) and an env kill-switch. If an engine
fails or the saving falls below the threshold, the original body is forwarded intact.

> **Superseded after release 0.11.0.** Both engines left the proxy for
> [`cervo-compress`](https://github.com/cervantesh/cervo-compress); deciding what a conversation
> may lose requires knowing that conversation, which a stateless gateway does not. What stayed
> is a transport-level size guard. See [P3-compression.md](specs/P3-compression.md) §2.

### D7 — Order: **P1 → P6 → (P2 ∥ P4) → P5 → P3**. Unanimous after arbitration.

- **P1 first and alone.** It is the only change that touches the hot path; it stabilises under
  `-race` and the coverage gate before anything is stacked on it.
- **P6 immediately after.** ~150 lines, and the only consumer that exercises SSE + headers +
  fallback by hand. If the trace cannot print "served by X, skipped Y (breaker), Z (quota)", it
  is badly designed — and that is worth discovering before Hermes parses it, not after.
- **P2 ∥ P4.** They share no line of code. P2 has the highest operational value; P4 is
  mechanical once the package is extracted.
- **P5** with Health + Counters + ring; it gains columns as P2 and P3 land.
- **P3 last.** It is the only one that mutates requests and the only one with silent
  degradation: it needs the trace (to audit what was compressed) and the dashboard (to see the
  saving and the regression) already working.

### D8 — P4: the interface first, three integrations, `Apply` writes with a backup.

An `Integration` interface with `Detect / Current / Apply / Verify / Revert`, extracted from
`cmd/doctor.go` along with the line-wise YAML helpers (`yamlScalar:102`, `yamlBlock:147`,
`yamlListEntries:221`), `checkResult:50` and `checkRoundTrip:359` — that last one is the "verify
it took effect" and is reused as is. `doctor` starts iterating the integration registry.

Initial cut: **Hermes + Claude Code + Codex**. Cursor/Cline/Aider later, as adapters of the same
interface — the value is validating the contract, not covering the catalogue.

A note on who writes: `doctor` writes nothing today (there is not a single
`os.WriteFile`/`os.Create`/`os.OpenFile` call in the file), and its YAML inspection is a
line-wise heuristic. **A heuristic that reads must not write.** So `Apply` really writes, with a
timestamped backup and `--revert`, where there is a reliable stdlib parser (Claude Code's JSON,
Codex's TOML); for Hermes/YAML it stays at printing the block and verifying the round trip. Hard
rule in every case: a marker-delimited block, and **never a parser round trip** — it destroys
the user's comments.

### D5b — Dashboard and chat, one line each

**P5:** `embed.FS` + plain HTML/JS behind the existing `admin` gate (`cmd/main.go:119`), 2 s
polling, no WebSockets. It computes nothing: every aggregate it shows must exist first as a
router snapshot. No historical series — `/metrics` already does that.

**P6:** `cmd/chat.go`, a stdlib REPL against the proxy's own
`/v1/{profile}/chat/completions`, no TUI framework. It is a client: it does not touch
`internal/router`.

---

## 2. What the human decides, not the architecture

**(a) Where P2's limits come from — the most important one.** If OpenRouter does not emit
`X-RateLimit-*` reliably on the free tier, what is left is manual config (which nobody
maintains) or learning from the 429 — which learns precisely the event P2 exists to avoid, and
which can also learn a false ceiling: a 429 may come from the per-minute limit rather than the
daily one, and `parseRetryAfter` does not distinguish them. The design supports all three
sources, but **which one is primary determines whether P2 genuinely prevents or merely labels
what already happens.** That is settled by an afternoon of measurement against the real account,
not by a design decision.

**(b) Soft vs hard as the quota exclusion default.** Excluding at 100 % of a window widens the
"all models cooling down" surface (more 503s with `Retry-After`) versus leaving the model last
in the ranking and taking the real 429. An honest predictive 503, or burning an attempt on a
likely 429? A proposal compatible with all three architects: soft by default, hard after N
confirmations or via env.

**(c) The shape of P1's contract toward Hermes.** Is the short form the only stable contract,
with the detail living behind `admin` only, or should Hermes be able to request the full JSON on
the hot path of every completion? That is an API decision toward the consumer, not internal
plumbing.

---

## 3. Irreversible risks

| Risk | Mitigation |
|---|---|
| P1's schema becomes a public API the moment Hermes parses it | Version in the first commit (`v1;…`), add-only fields, stable short form / evolving `full` |
| P3 can degrade answers **silently** | Opt-in per profile, `dry-run`, minimum threshold, `cmp=` always present, original body intact on any error |
| The quota scope (D4) is baked into the persisted file | `model` + `account` from day one |
| P4 can destroy the user's `settings.json` or `config.toml` | Timestamped backup + `--revert`, markers, never a parser round trip |
| Persisting the ring to disk (tempting for P5 across restarts) | **Forbidden**: they are conversation bodies. The durable series is `/metrics` |
| The lock-free ledger would break under speculative fan-out across models | The single-writer invariant is documented and covered by `-race` |
