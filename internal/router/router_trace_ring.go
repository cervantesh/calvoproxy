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

// recent returns the newest traces first, up to limit. Newest-first is the whole
// point: a dashboard showing the OLDEST 20 of 200 decisions is worse than
// useless, because it looks live while describing the past.
//
// The ring writes forward from r.next, so the newest entry is at next-1 and
// walking backwards yields recency order directly, wrap included.
func (r *traceRing) recent(limit int) []*routeTrace {
	if r == nil || limit < 1 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*routeTrace, 0, limit)
	size := len(r.entries)
	for i := 1; i <= size && len(out) < limit; i++ {
		idx := (r.next - i + size) % size
		if entry := r.entries[idx]; entry != nil {
			out = append(out, entry)
		}
	}
	return out
}

// RecentDecisions is the admin-channel view of the ring, newest first. It
// carries Reason because the only route that serves it is admin-gated — the same
// rule as /decisions/{id}: the gate authorises, not the path.
func (s *RouterService) RecentDecisions(limit int) []any {
	traces := s.traceRingRef().recent(limit)
	out := make([]any, 0, len(traces))
	for _, t := range traces {
		out = append(out, t.view(true))
	}
	return out
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

// traceView is the JSON shape of a trace, for both JSON channels. The rule that
// splits them (spec §6.1): closed identifiers travel everywhere, free-form text
// only behind the admin gate.
type traceView struct {
	ID string `json:"id"`
	// RuleID and RequestedModel are on both channels: the first is a closed
	// identifier, the second is what the caller itself asked for, so neither
	// tells an un-gated client anything it did not already know or hold.
	RuleID         string `json:"rule_id,omitempty"`
	RequestedModel string `json:"requested_model,omitempty"`
	// PolicyReason is the free text the matched rule carries. ADMIN CHANNEL
	// ONLY. Not for the §6 reason — it is authored by our own policy config, not
	// by an upstream — but because it narrates the authorisation logic, and the
	// opt-in channel has no gate in front of it. The generalised rule: closed
	// identifiers travel everywhere, free-form text only behind the admin gate.
	PolicyReason string             `json:"policy_reason,omitempty"`
	PolicySteps  []traceStepView    `json:"policy_steps,omitempty"`
	Profile      string             `json:"profile"`
	Outcome      string             `json:"outcome,omitempty"`
	Caps         []string           `json:"caps_required,omitempty"`
	Planned      int                `json:"planned"`
	Eligible     int                `json:"eligible"`
	Excluded     int                `json:"excluded_by_breaker,omitempty"`
	Attempts     []traceAttemptView `json:"attempts,omitempty"`
	Served       *traceAttemptView  `json:"served,omitempty"`
}

// traceStepView is one policy rule the engine looked at. Name and Matched are
// the structural part and travel on both channels; Detail follows PolicyReason
// and is admin-only.
type traceStepView struct {
	Name    string `json:"name"`
	Matched bool   `json:"matched"`
	Detail  string `json:"detail,omitempty"`
}

type traceAttemptView struct {
	Model  string  `json:"model"`
	Index  int     `json:"index"`
	Score  float64 `json:"score"`
	Status int     `json:"status,omitempty"`
	Kind   string  `json:"kind,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

// view renders the trace for one channel. adminChannel is what separates the
// two: everything free-form hangs off it, everything closed and structural is
// unconditional. See the field comments on traceView and spec §6.
func (t *routeTrace) view(adminChannel bool) traceView {
	if t == nil {
		return traceView{}
	}
	attempt := func(a traceAttempt) traceAttemptView {
		v := traceAttemptView{Model: a.Model, Index: a.Index, Score: a.Score, Status: a.Status, Kind: a.Kind}
		if adminChannel {
			v.Reason = a.Reason
		}
		return v
	}
	out := traceView{
		ID: t.ID, Profile: t.Profile, Outcome: t.Outcome, Caps: t.CapsRequired,
		RuleID: t.RuleID, RequestedModel: t.RequestedModel,
		Planned: t.Planned, Eligible: t.Eligible, Excluded: t.ExcludedByBreaker,
	}
	if adminChannel {
		out.PolicyReason = t.PolicyReason
	}
	for _, step := range t.PolicySteps {
		view := traceStepView{Name: step.Name, Matched: step.Matched}
		if adminChannel {
			view.Detail = step.Detail
		}
		out.PolicySteps = append(out.PolicySteps, view)
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
