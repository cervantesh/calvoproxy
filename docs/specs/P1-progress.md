# P1 — progress

One line per increment. Invariants numbered as in
[P1-decision-trace.md §8](P1-decision-trace.md).

| # | Invariant | Status | Note |
|---|---|---|---|
| 1 | Header before the first byte, SSE included | ✅ | Red first. Uses `headerSnapshotRecorder` |
| 2 | `PROXY_ROUTE_TRACE=off` changes nothing observable | ✅ | Red first. Off ⇒ no trace allocated |
| 3 | A `nil` trace is a no-op | ✅ | **No prior red**: nil-safety was already part of increment 1's design. Characterisation test |
| 4 | Header ≤ 512 bytes with deterministic trimming | ✅ | Red first |
| 5 | All four unserved exits emit a partial trace | ✅ | Red first, one subtest per path |
| 6 | gRPC inherits the header | ✅ | Red first, in `cmd/grpc_test.go` |
| 7 | Single-valued header | ✅ | Red first |
| 8 | No races under `-race` with concurrent streaming | ✅ | Red first — see below |
| 9 | The `full` opt-in carries no `Reason` | ✅ | Red first |
| 10 | `/decisions/{id}` carries `Reason`, behind `admin` | ✅ | Red first |
| 11 | Bounded ring, evicts the oldest | ✅ | Red first |

Gates: `go build -mod=vendor` · `go test -race ./...` · `coverage-gate.sh` — all three green.

## Where the spec was wrong

- **§8, invariant 7.** The draft claimed that with a duplicated header "the upstream's would
  win" because `cmd/grpc.go` takes `values[0]`. False: ours is written first, so ours comes
  first. The real defect is the duplication itself, with upstream text in the second value.
- **§4, the failure annotation point.** The draft put it in the fallback loop. Wrong place:
  `cervoretry.ClassifyHTTPStatus` remaps before the error gets there (500 → 502), so the trace
  would have reported a code the upstream never sent. The annotation moved to `executeAttempt`,
  the only point that sees the raw `resp.StatusCode`.

## What invariant 8 found

The first run produced three `DATA RACE` reports — but **not in the trace code**: the shared
`streamTransport` helper ([router_critical_path_test.go:32](../../internal/router/router_critical_path_test.go))
increments `calls` and writes `lastURL` without a lock. No test had shared it across goroutines
until now. The P1 test uses a stateless transport; the helper still carries the latent race and
deserves its own fix — out of scope for P1.

## Closing

All eleven invariants green, all three gates passing, load test unchanged, CHANGELOG and README
updated.

§5 and §6 (ring, `/decisions/{id}`, the `full` opt-in) landed in a final increment with their
own invariants 9–11. Deferring them to P5 — where the ring is needed for the dashboard anyway —
was considered, but cutting scope without anyone deciding to is worse than doing the work, so
they were implemented.

## Awaiting a human decision

- The header format freezes into an API the moment Hermes parses it. Still not explicitly
  approved; work proceeded on the draft.

## Out of scope, found along the way

- `X-Calvoproxy-Model`, `-Profile` and `-Attempt` have the same duplication exposure that was
  fixed for the new headers.
- The race in the `streamTransport` test helper.
