package router

import (
	"context"
	"net/http"
	"sync"
)

// traceRing keeps the last N decisions so /decisions/{id} can serve the detail
// the short header has no room for — the upstream reason text, and the stream
// outcome, which is only known after the headers are already on the wire.
//
// In memory and bounded, never on disk. These records are adjacent to
// conversation content, and a diagnostic buffer is not a place to accumulate it
// across restarts; /metrics is the durable series.
type traceRing struct {
	mu      sync.RWMutex
	entries []*routeTrace
	byID    map[string]*routeTrace
	next    int
}

func newTraceRing(capacity int) *traceRing {
	if capacity < 1 {
		capacity = 1
	}
	return &traceRing{
		entries: make([]*routeTrace, capacity),
		byID:    make(map[string]*routeTrace, capacity),
	}
}

func (r *traceRing) add(trace *routeTrace) {
	if r == nil || trace == nil || trace.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if evicted := r.entries[r.next]; evicted != nil {
		delete(r.byID, evicted.ID)
	}
	r.entries[r.next] = trace
	r.byID[trace.ID] = trace
	r.next = (r.next + 1) % len(r.entries)
}

func (r *traceRing) get(id string) (*routeTrace, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	trace, ok := r.byID[id]
	return trace, ok
}

// traceRingCapacity is read once per service so a running proxy has a stable
// buffer size.
func traceRingCapacity() int { return envInt("PROXY_TRACE_RING", 200) }

func (s *RouterService) traceRingRef() *traceRing {
	s.traceRingOnce.Do(func() { s.traces = newTraceRing(traceRingCapacity()) })
	return s.traces
}

// finishTrace publishes the request's trace to the ring. Called once, at the end
// of dispatchChain, from the request goroutine — the only writer, so the trace
// is fully settled by the time anyone else can read it.
func (s *RouterService) finishTrace(ctx context.Context) {
	trace := traceFrom(ctx)
	if trace == nil {
		return
	}
	s.traceRingRef().add(trace)
}

// Decision returns the recorded decision for an id, for the admin-gated
// /decisions/{id}. This is the ONE channel that carries the upstream reason
// text; see the spec's channel table.
func (s *RouterService) Decision(id string) (any, bool) {
	trace, ok := s.traceRingRef().get(id)
	if !ok {
		return nil, false
	}
	return trace.view(true), true
}

// traceView is the JSON shape of a trace. Reason is omitted unless the caller is
// on the admin channel, because it is upstream error body.
type traceView struct {
	ID       string             `json:"id"`
	Profile  string             `json:"profile"`
	Outcome  string             `json:"outcome,omitempty"`
	Caps     []string           `json:"caps_required,omitempty"`
	Planned  int                `json:"planned"`
	Eligible int                `json:"eligible"`
	Excluded int                `json:"excluded_by_breaker,omitempty"`
	Attempts []traceAttemptView `json:"attempts,omitempty"`
	Served   *traceAttemptView  `json:"served,omitempty"`
}

type traceAttemptView struct {
	Model  string  `json:"model"`
	Index  int     `json:"index"`
	Score  float64 `json:"score"`
	Status int     `json:"status,omitempty"`
	Kind   string  `json:"kind,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

func (t *routeTrace) view(includeReason bool) traceView {
	if t == nil {
		return traceView{}
	}
	attempt := func(a traceAttempt) traceAttemptView {
		v := traceAttemptView{Model: a.Model, Index: a.Index, Score: a.Score, Status: a.Status, Kind: a.Kind}
		if includeReason {
			v.Reason = a.Reason
		}
		return v
	}
	out := traceView{
		ID: t.ID, Profile: t.Profile, Outcome: t.Outcome, Caps: t.CapsRequired,
		Planned: t.Planned, Eligible: t.Eligible, Excluded: t.ExcludedByBreaker,
	}
	for _, a := range t.Attempts {
		out.Attempts = append(out.Attempts, attempt(a))
	}
	if t.Served != nil {
		served := attempt(*t.Served)
		out.Served = &served
	}
	return out
}

// --- client opt-in ---

type traceFullCtxKey struct{}

const traceOptInHeader = "X-Calvoproxy-Trace"
const traceJSONHeader = "X-Calvoproxy-Trace-Json"

// withTraceOptIn records that this client asked for the full JSON. Read from the
// request in RouteRequestWithProvider, because dispatchChain never sees *http.Request.
func withTraceOptIn(ctx context.Context, r *http.Request) context.Context {
	if r == nil || r.Header.Get(traceOptInHeader) != "full" {
		return ctx
	}
	return context.WithValue(ctx, traceFullCtxKey{}, true)
}

func traceFullRequested(ctx context.Context) bool {
	full, _ := ctx.Value(traceFullCtxKey{}).(bool)
	return full
}
