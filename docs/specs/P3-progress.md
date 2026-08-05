# P3 — progress

- **Invariants 1–10 green**, plus two integration tests through the router's real
  path. All three gates OK.
- **The coverage gate caught what review did not**: after hooking compression into
  `dispatchChain`, that function dropped from 92.9% to 92.1%, because the unit
  tests exercised the engines but nothing exercised the new branch through the
  real path. Proving an engine works and proving it is plugged in are two
  different claims; added a test that compresses end to end and its opposite with
  compression off.
- **A decision the spec did not anticipate**: `safeCompress` with `recover()`.
  Bodies come from clients and will eventually contain everything; a failure here
  must degrade to "not compressed", never to a 500.
- **Still off by default.** Without `PROXY_COMPRESS_PROFILES` nothing is touched,
  and the default path has its own test so it cannot regress.
- Before turning it on for real: run with `PROXY_COMPRESS_DRYRUN=true` for a while
  and compare the measured saving against answer quality. The design allows it;
  the judgement is human.

## Scope correction (after the 0.11.0 release)

- The user pointed out that context compression **does not look like a proxy's
  job**, and was right. The other five points observe, decide or report; this one
  mutated the request, and did so with less information than any of the clients
  had.
- **The panel had already flagged it in round 1** and I did not listen hard
  enough: one of the three said that of the six points "the one worth least as
  framed in the brief is P3". I cut the scope but built it anyway.
- Both engines moved to `github.com/cervantesh/cervo-compress`, where Hermes can
  import them. `dedup` left the proxy entirely; `toolcap` stayed, reframed as a
  transport guard governed by `PROXY_TOOL_RESULT_LIMIT` instead of
  `PROXY_COMPRESS_*`.
- The library also took the adversarial corpus (unclosed fences, nested JSON,
  blocks differing by one byte, Anthropic structured content), which was the part
  that least belonged in a gateway's test suite.
