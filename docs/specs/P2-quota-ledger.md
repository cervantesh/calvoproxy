# P2 — Per-model and per-account quota budget

Reference architecture: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md).

## 1. Problem

Free-tier limits are discovered by **hitting them**: a 429 arrives, the breaker opens the
circuit, and the request is already spent. The breaker is reactive by design and should stay
that way. What is missing is the predictive half: knowing how much of the window is left and
degrading *before* it runs out.

## 2. The key is not `breakerKey`

`breakerKey` is `profile + ":" + model` ([router_breaker.go:189](../../internal/router/router_breaker.go)),
and `allKnownAttempts` produces one entry **per profile-model pair**. That is right for
reliability: the same model under `coding` and under `bulk` sees different load and deserves
its own circuit and score.

For quota it is **fatal**. OpenRouter's allowance is per account and per model, not per
profile: with the same slug in several profiles — which is the case today in
`model-policy.json` — two partial counters of the same pocket would never detect exhaustion,
each seeing half the traffic.

```go
type quotaScope string // "model:<slug>" | "account"
```

The `account` scope is mandatory from day one: it is the free tier's dominant limit, and
adding it later would invalidate the already-persisted file.

## 3. Why a separate ledger and not the breaker

Writing `state.OpenUntil = windowReset` to inherit `filterAvailableAttempts` looks like
economy and is a bug:

1. `recordSuccess` ([:180](../../internal/router/router_breaker.go)) and `resolveProbe` ([:160](../../internal/router/router_breaker.go))
   set `OpenUntil = time.Time{}` **unconditionally**. A request in flight that ends in a 200
   would silently erase the quota exclusion just placed.
2. `recordFailure` resets `ConsecutiveFailures` once the window expires: a daily window would
   hold the half-open machinery hostage for 24 h.
3. `Health()` ([:302](../../internal/router/router_breaker.go)) classifies any future
   `OpenUntil` as `"open"` and degrades `Status` to `unavailable`: a spent budget would be
   reported as a fault.

Its own ledger, with its own mutex. **Mandatory lock order: `breakerMu → quotaMu`**, because
`isModelAvailableLocked` is reached from `Health()` with `breakerMu` already held. The ledger
must never call anything that takes `breakerMu`.

## 4. Where the limits come from

By priority, and **none of them invents a ceiling**:

1. **Explicit configuration** in `PROXY_QUOTA_LIMITS_JSON`
   (`{"model:openai/gpt-oss-20b:free":{"rpd":50},"account":{"rpd":1000}}`).
   `model-policy.json` is not touched: its shape is owned by the vendored policy package.
2. **Upstream headers** `X-RateLimit-Limit` / `-Remaining` / `-Reset` when they arrive.
3. **Learning from a 429 with `Retry-After`**, which sets `ResetAt` but **not** a `Limit`: a
   429 says "not now", it does not say how many fit.

With no known limit there is no gate: the ledger counts but `headroom` is 1. Faking a ceiling
would be worse than having none.

## 5. Degradation

- **Soft, by default.** `rankAttemptsByScore` orders by `score × headroom`, with
  `headroom ∈ [0,1]` derived from the percentage of the window consumed. It **does not touch
  the persisted score**: the score measures reliability, not budget, and contaminating it
  would poison its two-clock decay.
- **Hard, only under `PROXY_QUOTA_HARD_SKIP=true`.** At 100 % of a window the model is
  excluded, with reason `quota` in P1's trace and `Retry-After` set to the minimum of the
  breaker cooldown and the window's `ResetAt`.

The default is soft because hard exclusion widens the "all cooling" 503 surface on the
strength of limits that may have been learned, and therefore may be inaccurate.

## 6. Persistence

Its **own** file, `quotas.json`, beside `scores.json`. It does not go into the score store:
its expiry is `ResetAt`, incompatible with `defaultScoreMaxAge`
([router_scoring_store.go:30](../../internal/router/router_scoring_store.go)), which discards
the whole file after 24 h; and `restoreScores` filters by `knownBreakerKeys()`, keys quota does
not use. The atomic temp+rename helper is shared, the file is not.

On load: if `ResetAt` has passed, the window returns to zero — it is **not discarded**. A daily
counter has to survive a proxy restart, because the upstream's window does not reset just
because ours did.

## 7. Verifiable invariants

| # | Invariant | How it is tested |
|---|---|---|
| 1 | Quota is keyed by bare model: the same slug in two profiles shares one counter | consume under two profiles; the counter is the sum |
| 2 | The `account` scope counts every request, whatever the model | two different models; `account` sums them |
| 3 | Past `ResetAt` the window returns to zero rather than being discarded | expired window; `Used` is 0 and the limit survives |
| 4 | Quota persists and restores; a window that already rolled loads at zero | save, reload |
| 5 | Soft degradation reorders but does **not** alter the persisted score | identical score before and after |
| 6 | With no known limit, `headroom` is 1 and there is no gate | model with no configured limit |
| 7 | Hard exclusion only happens under `PROXY_QUOTA_HARD_SKIP` | same state, both configurations |
| 8 | A 429 still opens the breaker: quota does not replace it | 429; assert `ConsecutiveFailures` |
| 9 | `Health()` holding `breakerMu` and consulting the ledger does not deadlock | concurrent load under `-race` |
| 10 | A 429 with `Retry-After` sets `ResetAt` but invents no `Limit` | after learning, `Limit` is still 0 |

## 7b. Corrections to this spec during implementation

1. **"With no known limit, headroom is 1" was incomplete.** `headroom` is the minimum across
   *every* applicable window, and the **account** budget legitimately constrains a model with
   no ceiling of its own. Isolating invariant 6's case requires a ledger with no configured
   limits at all, not merely no model limit. A sibling invariant was added asserting the
   opposite: with `account` configured and the model uncapped, headroom is set by the account.

2. **The trace counted quota exclusions as if they were the breaker's.**
   `ExcludedByBreaker` was derived by subtraction (`afterCaps - eligible`), so with hard skip
   on, a spent budget was reported as an open circuit — exactly the ambiguity P1 exists to
   remove. Added `ExcludedByQuota` and a `q=` header field, and the breaker's subtraction now
   discounts it. This **changes P1's contract**, additively: `q=` is a new optional field
   within `v1`.

## 8. Out of scope

Coordination across replicas (the ledger is local, like the scores) and per-token rather than
per-request quotas: the free tier limits requests, and counting tokens would mean trusting the
`usage` field of every response, which does not always arrive.
