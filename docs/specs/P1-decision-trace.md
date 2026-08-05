# P1 — Routing decision trace

Reference architecture: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md).

## 1. Problem

`setServedModelHeaders` ([router_http.go:76](../../internal/router/router_http.go)) already
says **which** model answered (`X-Calvoproxy-Model`, `-Profile`, `-Attempt`). It does not say
**why**: which models were dropped and for what reason, what score the chain was ordered by,
which attempts failed first and with what code. Today that lives only in a `slog.InfoContext`
in `dispatchChain` ([router.go:311](../../internal/router/router.go)) and is lost to the client.

## 2. Trace schema

Internal structure, one pointer per request, in `internal/router/router_trace.go`.

```go
type routeTrace struct {
    ID        string    // 16 hex chars, crypto/rand
    StartedAt time.Time

    Profile        string   // as already resolved by determineProfile
    RequestedModel string   // "" when the client pinned no model
    RuleID         string   // policyDecision.RuleID
    CapsRequired   []string // nil when no vision/tools were required

    Planned   int // len after planModelAttempts
    AfterCaps int // len after applyCapabilityFilter
    Eligible  int // len after filterAvailableAttempts + rank + truncation

    Excluded []traceExclusion
    Attempts []traceAttempt

    Served      *traceAttempt     // nil when no attempt succeeded
    Compression *traceCompression // nil until P3; the header says cmp=off
    Outcome     string            // served | all_cooling | caps_none | caps_pinned | chain_failed
}

type traceExclusion struct {
    Model string
    Why   string    // breaker | capability
    Until time.Time // breaker only; zero otherwise
}

type traceAttempt struct {
    Model  string
    Index  int     // 1-based, same as modelAttempt.AttemptIndex
    Score  float64 // score at the moment the chain was ordered
    Status int     // attemptError.StatusCode; 200 on the served one
    Kind   string  // ok | http | transport | skip | unavailable | stream_abort | probe_busy
    Reason string  // ADMIN CHANNEL ONLY — see §6
    Millis int64
}
```

`Kind` is a closed enum. It is the field that travels on the public channels; `Reason` is
free-form text of upstream origin and does not.

## 3. Short header format

Header `X-Calvoproxy-Route`, **single-valued**, ASCII, `;` between fields and `,` between
entries of a list. The first field is always the version.

```
X-Calvoproxy-Route: v1;p=coding;s=0.83;a=2;n=4/4/3;caps=tools;prev=gpt-oss-20b:429,gemma-4-31b:skip;brk=1;cmp=off
```

| Field | Present | Meaning |
|---|---|---|
| `v1` | always, first | format version |
| `p=` | always | resolved profile |
| `s=` | when there is a served model | the served model's score at ordering time, 2 decimals |
| `a=` | when `AttemptIndex > 1` | position in the chain (the degradation signal) |
| `n=` | always | `Planned/AfterCaps/Eligible` |
| `caps=` | when required | `tools`, `vision` or `tools+vision` |
| `prev=` | when there were failures | `model:code` per failed attempt, in order |
| `brk=` | when the breaker excluded models | number of models excluded |
| `q=` | when quota excluded models (P2) | number of models out of budget |
| `cmp=` | **always** | `off` until P3; afterwards `-3.1k` or similar |
| `o=` | when `Outcome != served` | the value of `Outcome` |
| `trunc=1` | when trimmed | see §3.2 |

**Model names are abbreviated** in the header: the organisation prefix and the `:free` suffix
are dropped. `nvidia/nemotron-3-super-120b-a12b:free` → `nemotron-3-super-120b-a12b`. The full
name already travels in `X-Calvoproxy-Model` and in the JSON.

### 3.1 Codes in `prev=`

An HTTP integer, or one of these literals when there is no meaningful HTTP status: `skip`
(`SkipModel`), `unavail` (`isModelUnavailable`), `probe` (half-open probe busy), `trans`
(transport error), `stream` (stream aborted).

### 3.2 Hard cap of 512 bytes

If the value exceeds 512 bytes it is trimmed **deterministically**, in this order, until it
fits:

1. drop `prev=` entries from the end, one at a time;
2. if it still does not fit, drop `prev=` entirely;
3. if it still does not fit, drop `brk=`/`q=` and `n=`.

After any trimming, `;trunc=1` is appended. The fields `v1`, `p=`, `cmp=` and `o=` are never
dropped: if those alone exceeded 512 bytes it would be a bug, and invariant 4's test covers it
with the worst constructible case.

## 4. Annotation points

| Where | What it writes |
|---|---|
| `dispatchChain` ([router.go:263](../../internal/router/router.go)) | creates the trace and puts it in the `ctx`; `Profile`, `RequestedModel`, `RuleID`, `CapsRequired` |
| after `planModelAttempts` (:286) | `Planned` |
| `applyCapabilityFilter` (:372) | `AfterCaps` + one `traceExclusion{Why:"capability"}` per drop |
| after `filterAvailableAttempts` (:303) | `traceExclusion{Why:"breaker", Until}` per exclusion |
| `rankAttemptsByScore` ([router_scoring.go:230](../../internal/router/router_scoring.go)) | each model's score, already computed there — no second pass |
| after truncation to `MaxAttempts` (:307) | `Eligible` |
| `executeAttempt`, every failure path ([router_upstream.go](../../internal/router/router_upstream.go)) | one `traceAttempt` per failure, with the **raw** upstream status |
| `executeAttempt`, non-streaming success ([router_upstream.go:252](../../internal/router/router_upstream.go)) | `Served` |
| `executeAttempt`, streaming success (:209) | `Served`, before `setServedModelHeaders` |
| `setServedModelHeaders` ([router_http.go:76](../../internal/router/router_http.go)) | materialises `X-Calvoproxy-Route` and `X-Calvoproxy-Decision-Id` |
| the four unserved exits (:282, :299, :339, :366) | `Outcome` + materialises the partial trace before `writeJSONError` |

**The trace travels in the `context.Context` under a typed key.** Not as a field of
`FallbackExecution`: `setServedModelHeaders` is called from **inside** `executeAttempt`
(`router_upstream.go:209` and `:259`), and `AttemptExecutor.ExecuteAttempt`
([router_types.go:157](../../internal/router/router_types.go)) does not receive
`FallbackExecution`. Verified.

Accessor `traceFrom(ctx) *routeTrace`, returning `nil` out of band. Every annotation method is
a no-op on a `nil` receiver, following the pattern of `s.capabilities != nil`
([router.go:618](../../internal/router/router.go)).

### 4.1 Concurrency

A single writer: the request's goroutine. `streamCopy`
([router_stream.go:97](../../internal/router/router_stream.go)) and `awaitFirstStreamEvent`
(`:236`) **do not** touch the trace. The stream outcome is consolidated in the `switch` at
`router_upstream.go:213-229`, which runs on the request goroutine — but **after** the headers
are sent, so it can never be reflected in the header. It is reflected in the ring copy, and
therefore in `/decisions/{id}`.

When the request closes, a compacted copy goes to the ring. The live pointer is never
published.

## 5. Ring and `/decisions/{id}`

A circular in-memory ring on `RouterService`, fixed size `PROXY_TRACE_RING` (default 200), with
its own mutex. Never persisted to disk: these are conversation bodies.

`GET /decisions/{id}` behind the same `admin` gate as `/health`
([cmd/main.go:119](../../cmd/main.go)). Returns the full JSON, **with** `Reason`. `404` if the
id has already rotated out of the ring.

## 6. Sanitisation and channels

Three channels with different content. The rule is that **`Reason` exists only on the admin
channel**, because it is upstream error body (`truncateReason` cuts it to 240 bytes,
[router_breaker.go:559](../../internal/router/router_breaker.go), but does not sanitise it).

| Channel | Gate | Contains |
|---|---|---|
| `X-Calvoproxy-Route` | none | short form; codes and enums only |
| `X-Calvoproxy-Trace: full` (client opt-in) | none | full JSON **without** `Reason` |
| `GET /decisions/{id}` | `admin` | full JSON **with** truncated `Reason` |

Every value entering the header is filtered to `[A-Za-z0-9._/+-]`; any other byte becomes `_`.
A model name comes from config, but `RequestedModel` comes from the client and must not be able
to inject CR/LF or separators.

## 7. Disabling

`PROXY_ROUTE_TRACE=off` → `dispatchChain` creates no trace, `traceFrom` returns `nil`, no new
header is emitted and the ring is untouched. Observable behaviour goes back to being byte-for-
byte what it is today (invariant 2).

## 8. Verifiable invariants

| # | Invariant | How it is tested |
|---|---|---|
| 1 | The header goes out **before** the first byte of the body, SSE included | `headerSnapshotRecorder` ([router_critical_path_test.go:68](../../internal/router/router_critical_path_test.go)), like `TestStreaming_ServedModelHeadersPrecedeTheBody` |
| 2 | With `PROXY_ROUTE_TRACE=off` nothing observable changes | same request with and without the env; compare status, body and header set |
| 3 | A `nil` trace is a no-op at every annotation point | direct call to each method on a `nil` receiver, no panic |
| 4 | The header never exceeds 512 bytes | chain with the maximum models, all failing, longest names and reasons; assert `len <= 512` and `trunc=1` |
| 5 | All four unserved exits emit a partial trace with their `o=` | one test per path: pinned-capability 422, capability 503, all-cooling 503, chain failure |
| 6 | gRPC inherits the header with no new work in the router | gRPC request; assert `X-Calvoproxy-Route` in the response's `Headers` map |
| 7 | Single-valued: the header is not duplicated when copying the upstream's | upstream that emits `X-Calvoproxy-Route`; assert one value after `streamProxyResponse` |
| 8 | No race under `-race` with concurrent streaming | N parallel requests, `go test -race` |
| 9 | `X-Calvoproxy-Trace: full` returns the full JSON **without** `Reason`; without the opt-in nothing is emitted | one request with the header and one without |
| 10 | `/decisions/{id}` returns the trace **with** `Reason`; an unknown id ⇒ not found | lookup after a real request, and with an invented id |
| 11 | The ring is bounded and evicts the oldest traces | small `PROXY_TRACE_RING`, more requests than capacity |

The response header carrying the opt-in JSON is `X-Calvoproxy-Trace-Json`. It is a separate
header from `X-Calvoproxy-Route` on purpose: the short form is the stable contract and must not
change size or shape because a client asked for the detail.

Invariant 7 was not in the goal: this spec adds it because `streamProxyResponse` calls
`copyHeaders` with `dst.Add` ([router_http.go:58](../../internal/router/router_http.go))
**after** `setServedModelHeaders` has written ours. An upstream emitting the same header
produces two values. `x-calvoproxy-route` and `x-calvoproxy-decision-id` have to be added to
that call's `skipKeys`.

> **Correction (phase B).** The first draft of this spec claimed that in that scenario "the
> upstream's would win", because `cmd/grpc.go:104` takes `values[0]`. That is **false**, and it
> was checked by running the test: the observed values are
> `["v1;p=simple;cmp=off", "v1;p=INJECTED"]` — ours comes first, because
> `setServedModelHeaders` runs before the `copyHeaders`. gRPC therefore already takes the right
> one.
>
> The real defect is the **duplication**: the client receives two values of the same header,
> the second with content the upstream controls. A client that reads all values, or keeps the
> last, or whose stack joins them with commas, consumes foreign text as if it were a routing
> decision from the proxy. The invariant stands unchanged — one value, and the upstream's must
> not survive — but its justification was wrong.
>
> This already affects `X-Calvoproxy-Model`, `-Profile` and `-Attempt` today, which have
> exactly the same problem. It stays **out of scope for P1**: this spec's `skipKeys` covers
> only the two new headers, and the three old ones deserve their own change and their own test.

> **Correction (phase B).** The draft put the failure annotation in
> `DefaultFallbackExecutor.Execute`'s loop. Wrong place: by the time the error reaches the
> loop, `classifyHTTPError` has already run it through `cervoretry.ClassifyHTTPStatus`, which
> **remaps** — an upstream 500 arrives at the loop as a 502. A trace reporting `model-a:502`
> when OpenRouter said 500 misleads exactly the person trying to work out what really happened.
> `executeAttempt` is the only point that sees `resp.StatusCode` unremapped, so the annotation
> lives there. The loop annotates nothing.

## 9. Out of scope

The stream's outcome (`streamCompleted` / aborted) cannot be in the header: it is known after
the header is sent. It lives only in the ring and in `/decisions/{id}`. Any consumer that needs
the outcome queries that endpoint; the header's contract will not be broken to squeeze it in.
