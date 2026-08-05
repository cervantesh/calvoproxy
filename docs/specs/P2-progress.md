# P2 — progress

- **Invariants 1–10 green**, plus four siblings that emerged while implementing.
  All three gates pass.
- **The spec was wrong in two places**, corrected in the document itself (§7b)
  before the code:
  1. "With no known limit, headroom is 1" ignored that the *account* budget
     constrains a model with no ceiling of its own. The original test failed for
     good reason.
  2. The trace counted quota exclusions as breaker exclusions, because
     `ExcludedByBreaker` was a derived subtraction. With hard skip on, a spent
     budget was reported as a broken circuit. Added `q=`.
- **A decision still waiting on real data**: where the limits come from. The
  ledger supports config, headers and learning from a 429, but which one is
  primary depends on whether OpenRouter emits `X-RateLimit-*` reliably on the free
  tier. Until that is measured, without `PROXY_QUOTA_LIMITS_JSON` the gate does
  nothing: it counts but never degrades. That is deliberate — inventing a ceiling
  would be worse.
- The default is **soft**: `PROXY_QUOTA_HARD_SKIP` stays off.
- **CI caught a flaky test that always passed locally.**
  `TestQuota_SoftDegradationLeavesScoreUntouched` compared two reads of
  `scoreForAttempt` for exact equality, and that function applies time-based decay
  on every call: the observed difference was ~1.5e-10, pure clock. It passed on
  the laptop because the laptop is faster than the runner. It now reads the
  **stored** score under the lock, which is the honest measurement of the
  invariant ("quota does not write to the score"). Lesson: any exact-equality
  assertion on a decaying value is flaky by construction.
