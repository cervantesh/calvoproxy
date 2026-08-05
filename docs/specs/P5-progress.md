# P5 — progress

- **Invariants 1–6 green.** Eight tests across `cmd/dashboard_test.go` and
  `internal/router/router_trace_recent_test.go`. All three gates OK.
- **The only thing the router needed** was `traceRing.recent(n)`: the ring could
  look up by id but not list. It was tested there, not in the view — the rule
  that the dashboard computes nothing holds by itself once anything it needs has
  to be added where the tests actually are.
- **A decision the spec did not anticipate**: the `Content-Security-Policy`
  header with `default-src 'self'`. Nobody asked for it, but it turns invariant 5
  ("nothing external") into something that also fails in the browser, not only in
  a test someone could delete.
- Pending: the compression column will appear on its own once P3 fills in `cmp=`.
