package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Invariant 1 (docs/specs/P1-decision-trace.md §8): the route-decision header
// must be on the wire BEFORE the first byte of the body, streaming included.
//
// Uses headerSnapshotRecorder rather than a plain httptest.ResponseRecorder for
// the reason documented at router_critical_path_test.go:63 — the plain recorder
// hands out one live header map, so a header set too late still shows up and the
// ordering assertion passes in false.
func TestTrace_RouteHeaderPrecedesStreamBody(t *testing.T) {
	upstream := &streamTransport{
		events: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
	}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	rec := newHeaderSnapshotRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[],"stream":true}`), "k", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("stream should return 200, got %d: %s", rec.Code, rec.Body.String())
	}

	route := rec.sentHeader("X-Calvoproxy-Route")
	if route == "" {
		t.Fatal("X-Calvoproxy-Route missing from the committed headers: the client " +
			"streamed an answer with no way to know why this model was chosen")
	}
	if !strings.HasPrefix(route, "v1;") {
		t.Errorf("route header must open with the format version, got %q", route)
	}
	if !strings.Contains(route, "p=simple") {
		t.Errorf("route header must carry the resolved profile, got %q", route)
	}
	// cmp= is present even with no compression, so "not compressed" is
	// distinguishable from "the field does not exist" (spec §3).
	if !strings.Contains(route, "cmp=") {
		t.Errorf("route header must always carry cmp=, got %q", route)
	}
	if rec.sentHeader("X-Calvoproxy-Decision-Id") == "" {
		t.Error("X-Calvoproxy-Decision-Id missing: /decisions/{id} would be unreachable")
	}
}

// Invariant 7 (spec §8): exactly one value for the route header, even when the
// upstream emits one too. streamProxyResponse copies upstream headers with
// dst.Add AFTER setServedModelHeaders has written ours.
func TestTrace_RouteHeaderIsSingleValued(t *testing.T) {
	extra := http.Header{}
	extra.Set("X-Calvoproxy-Route", "v1;p=INJECTED")
	upstream := &streamTransport{
		events: "data: [DONE]\n\n",
		extra:  extra,
	}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	rec := newHeaderSnapshotRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[],"stream":true}`), "k", "")

	values := rec.sent.Values("X-Calvoproxy-Route")
	t.Logf("observed values: %#v", values)
	if len(values) != 1 {
		t.Fatalf("route header must be single-valued, got %d values: %#v", len(values), values)
	}
	if strings.Contains(values[0], "INJECTED") {
		t.Errorf("upstream value must not survive, got %q", values[0])
	}
}

// Invariant 2 (spec §7): with PROXY_ROUTE_TRACE=off nothing observable changes.
// The trace is a diagnostic, so it must be possible to take it out of the
// picture entirely — not merely to blank the header.
func TestTrace_DisabledLeavesResponseUnchanged(t *testing.T) {
	respond := func(t *testing.T) *headerSnapshotRecorder {
		t.Helper()
		upstream := &streamTransport{events: "data: [DONE]\n\n"}
		svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
			DefaultProfile: "simple",
			Profiles:       map[string][]string{"simple": {"model-a"}},
			Aliases:        map[string]string{"default": "simple", "simple": "simple"},
		})
		rec := newHeaderSnapshotRecorder()
		svc.RouteRequestWithProvider(rec, trustedRequest(
			http.MethodPost, "/v1/chat/completions", `{"messages":[],"stream":true}`), "k", "")
		return rec
	}

	on := respond(t)
	t.Setenv("PROXY_ROUTE_TRACE", "off")
	off := respond(t)

	if off.Code != on.Code {
		t.Errorf("status changed with tracing off: %d vs %d", off.Code, on.Code)
	}
	if off.Body.String() != on.Body.String() {
		t.Errorf("body changed with tracing off")
	}
	if got := off.sentHeader("X-Calvoproxy-Route"); got != "" {
		t.Errorf("route header must be absent with tracing off, got %q", got)
	}
	if got := off.sentHeader("X-Calvoproxy-Decision-Id"); got != "" {
		t.Errorf("decision id must be absent with tracing off, got %q", got)
	}
	// Everything the proxy said before P1 must still be said.
	for _, h := range []string{"X-Calvoproxy-Model", "X-Calvoproxy-Profile"} {
		if off.sentHeader(h) != on.sentHeader(h) {
			t.Errorf("%s changed with tracing off: %q vs %q", h, off.sentHeader(h), on.sentHeader(h))
		}
	}
}

// Invariant 3 (spec §4): a nil trace is a no-op everywhere. Callers annotate
// unconditionally; out-of-band callers (a direct executor call in a test, or a
// replaced FallbackExecutor) must not panic.
func TestTrace_NilReceiverIsNoOp(t *testing.T) {
	var trace *routeTrace
	if got := trace.header(); got != "" {
		t.Errorf("nil trace must render an empty header, got %q", got)
	}
	// traceFrom on a context that never carried one.
	if got := traceFrom(context.Background()); got != nil {
		t.Errorf("traceFrom must return nil out of band, got %#v", got)
	}
	// withTrace(nil) must not wrap the context.
	if ctx := withTrace(context.Background(), nil); traceFrom(ctx) != nil {
		t.Error("withTrace(nil) must leave the context without a trace")
	}
	// Emitting from a nil trace writes nothing rather than blank headers.
	h := http.Header{}
	setRouteTraceHeaders(h, nil)
	if len(h) != 0 {
		t.Errorf("nil trace must write no headers, got %v", h)
	}
}

// Invariant 5 (spec §8): the four exits that never serve a model still emit a
// partial trace carrying o=<outcome>. These are exactly the requests where the
// caller most needs to know why — an error with no explanation is the state P1
// exists to remove.
func TestTrace_PartialTraceOnEveryUnservedExit(t *testing.T) {
	visionBody := `{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`

	cases := []struct {
		name    string
		outcome string
		status  int
		setup   func(t *testing.T, svc *RouterService)
		body    string
	}{
		{
			name:    "pinned model lacks the capability",
			outcome: "caps_pinned",
			status:  http.StatusUnprocessableEntity,
			setup: func(_ *testing.T, svc *RouterService) {
				svc.capabilities = newCapabilityIndex(map[string][]string{"model-a": {"tools"}})
			},
			body: `{"model":"model-a","messages":[{"role":"user","content":[` +
				`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`,
		},
		{
			name:    "no capable model anywhere",
			outcome: "caps_none",
			status:  http.StatusServiceUnavailable,
			setup: func(_ *testing.T, svc *RouterService) {
				svc.capabilities = newCapabilityIndex(map[string][]string{"model-a": {}})
			},
			body: visionBody,
		},
		{
			name:    "every model cooling down",
			outcome: "all_cooling",
			status:  http.StatusServiceUnavailable,
			setup: func(_ *testing.T, svc *RouterService) {
				key := svc.breakerKey(modelAttempt{Profile: "simple", Model: "model-a"})
				svc.modelBreakers[key] = &modelBreakerState{OpenUntil: time.Now().Add(time.Minute)}
			},
			body: `{"messages":[]}`,
		},
		{
			name:    "chain exhausted upstream",
			outcome: "chain_failed",
			status:  http.StatusBadGateway,
			setup:   func(_ *testing.T, _ *RouterService) {},
			body:    `{"messages":[]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := failingTransport{}
			svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
				DefaultProfile: "simple",
				Profiles:       map[string][]string{"simple": {"model-a"}},
				Aliases:        map[string]string{"default": "simple", "simple": "simple"},
			})
			tc.setup(t, svc)

			rec := httptest.NewRecorder()
			svc.RouteRequestWithProvider(rec, trustedRequest(
				http.MethodPost, "/v1/chat/completions", tc.body), "k", "")

			if rec.Code != tc.status {
				t.Fatalf("expected %d, got %d: %s", tc.status, rec.Code, rec.Body.String())
			}
			route := rec.Header().Get("X-Calvoproxy-Route")
			if route == "" {
				t.Fatalf("no trace on an unserved exit: the caller gets an error with no reason")
			}
			if !strings.Contains(route, "o="+tc.outcome) {
				t.Errorf("expected o=%s in %q", tc.outcome, route)
			}
			if strings.Contains(route, "s=") {
				t.Errorf("no model was served, so no score may be reported: %q", route)
			}
		})
	}
}

// failingTransport answers every attempt with a 502 so the chain exhausts.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"upstream down"}`)),
	}, nil
}

// The header must carry the decision, not just the profile: which attempt
// served, the score it was ranked by, how the chain narrowed, and which models
// failed first with what code. Without these the trace answers "who" but not
// "why", which is the whole point of P1.
func TestTrace_HeaderCarriesTheDecision(t *testing.T) {
	upstream := &sequenceTransport{statuses: []int{http.StatusInternalServerError, http.StatusOK}}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a", "model-b"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[]}`), "k", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("second model should have served, got %d: %s", rec.Code, rec.Body.String())
	}
	route := rec.Header().Get("X-Calvoproxy-Route")
	for _, want := range []string{
		"a=2",              // served by a fallback: the degraded signal
		"n=2/2/2",          // planned / after caps / eligible
		"prev=model-a:500", // the UPSTREAM status, not the remapped 502
		"s=",               // the score the chain was ranked by
	} {
		if !strings.Contains(route, want) {
			t.Errorf("route header missing %q, got %q", want, route)
		}
	}
}

// Invariant 4 (spec §3.2): the header never exceeds 512 bytes. Over the cap it
// is trimmed deterministically — prev= entries from the end first — and says so
// with trunc=1, so a consumer never mistakes a trimmed trace for a short chain.
func TestTrace_HeaderStaysUnder512Bytes(t *testing.T) {
	// A long chain of long slugs, every one of them failing: the worst case the
	// renderer can be handed.
	long := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		long = append(long, "someorg/a-deliberately-long-free-model-slug-number-"+
			string(rune('a'+i))+"-120b-a12b:free")
	}
	svc := newTestService(t, &http.Client{Transport: failingTransport{}}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": long},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[]}`), "k", "")

	route := rec.Header().Get("X-Calvoproxy-Route")
	if n := len(route); n > 512 {
		t.Fatalf("route header is %d bytes, over the 512 cap: %q", n, route)
	}
	if !strings.Contains(route, "trunc=1") {
		t.Errorf("a trimmed header must say so with trunc=1, got %q", route)
	}
	// The fields that never get dropped.
	for _, want := range []string{"v1", "p=simple", "cmp=", "o="} {
		if !strings.Contains(route, want) {
			t.Errorf("trimming dropped a protected field %q: %q", want, route)
		}
	}
}

// Invariant 8 (spec §4.1): the trace is lock-free because exactly one goroutine
// writes it. That is an invariant, not a hope — this is the test that fails if
// anyone ever annotates from streamCopy or awaitFirstStreamEvent. Meaningful
// only under -race, which the build gate runs.
func TestTrace_NoRaceUnderConcurrentStreams(t *testing.T) {
	// A stateless transport on purpose: the shared streamTransport helper counts
	// calls without a lock, so reusing it here would report ITS race instead of
	// exercising the trace's single-writer invariant.
	svc := newTestService(t, &http.Client{Transport: concurrentStreamTransport{}}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a", "model-b"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			svc.RouteRequestWithProvider(rec, trustedRequest(
				http.MethodPost, "/v1/chat/completions", `{"messages":[],"stream":true}`), "k", "")
			if rec.Header().Get("X-Calvoproxy-Route") == "" {
				t.Error("concurrent request lost its trace")
			}
		}()
	}
	wg.Wait()
}

const streamEventsFixture = `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
	`data: [DONE]` + "\n\n"

// concurrentStreamTransport holds no mutable state, so many goroutines may share
// one instance.
type concurrentStreamTransport struct{}

func (concurrentStreamTransport) RoundTrip(*http.Request) (*http.Response, error) {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(streamEventsFixture)),
	}, nil
}

// Invariant 9 (spec §6 and §8): the full JSON travels only when the client asks
// for it, and never carries Reason — that is upstream error text, and this
// channel has no admin gate in front of it.
func TestTrace_FullOptInOmitsUpstreamText(t *testing.T) {
	newSvc := func(t *testing.T) *RouterService {
		return newTestService(t, &http.Client{Transport: failingTransport{}}, policyConfig{
			DefaultProfile: "simple",
			Profiles:       map[string][]string{"simple": {"model-a"}},
			Aliases:        map[string]string{"default": "simple", "simple": "simple"},
		})
	}

	plain := httptest.NewRecorder()
	newSvc(t).RouteRequestWithProvider(plain, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[]}`), "k", "")
	if got := plain.Header().Get("X-Calvoproxy-Trace-Json"); got != "" {
		t.Errorf("full JSON must not be emitted without the opt-in, got %q", got)
	}

	req := trustedRequest(http.MethodPost, "/v1/chat/completions", `{"messages":[]}`)
	req.Header.Set("X-Calvoproxy-Trace", "full")
	full := httptest.NewRecorder()
	newSvc(t).RouteRequestWithProvider(full, req, "k", "")

	raw := full.Header().Get("X-Calvoproxy-Trace-Json")
	if raw == "" {
		t.Fatal("opt-in requested but no full trace emitted")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("full trace is not valid JSON: %v (%q)", err, raw)
	}
	if strings.Contains(strings.ToLower(raw), "upstream down") {
		t.Error("upstream error text leaked through the un-gated channel")
	}
	if strings.Contains(raw, `"reason"`) {
		t.Errorf("reason must never appear outside /decisions/{id}: %q", raw)
	}
	if decoded["profile"] != "simple" {
		t.Errorf("full trace lost the profile: %q", raw)
	}
}

// Invariant 10 (spec §5): /decisions/{id} serves the same trace WITH the reason,
// because that endpoint sits behind the admin gate. An unknown id is simply not
// found — the ring is bounded and ids rotate out.
func TestTrace_DecisionLookupCarriesReason(t *testing.T) {
	svc := newTestService(t, &http.Client{Transport: failingTransport{}}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[]}`), "k", "")

	id := rec.Header().Get("X-Calvoproxy-Decision-Id")
	if id == "" {
		t.Fatal("no decision id to look up")
	}
	found, ok := svc.Decision(id)
	if !ok {
		t.Fatalf("decision %s not in the ring", id)
	}
	raw, err := json.Marshal(found)
	if err != nil {
		t.Fatalf("decision does not marshal: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(raw)), "upstream unavailable") {
		t.Errorf("the admin channel must retain a safe classified reason, got %s", raw)
	}
	if strings.Contains(strings.ToLower(string(raw)), "upstream down") {
		t.Errorf("the admin channel must not retain raw upstream error bodies, got %s", raw)
	}
	if _, ok := svc.Decision("ffffffffffffffff"); ok {
		t.Error("an unknown id must not resolve")
	}
}

// Invariant 11 (spec §5): the ring is bounded. It holds conversation-adjacent
// metadata, so it is capped and never persisted.
func TestTrace_RingDropsOldestBeyondCapacity(t *testing.T) {
	t.Setenv("PROXY_TRACE_RING", "3")
	svc := newTestService(t, &http.Client{Transport: failingTransport{}}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	ids := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		rec := httptest.NewRecorder()
		svc.RouteRequestWithProvider(rec, trustedRequest(
			http.MethodPost, "/v1/chat/completions", `{"messages":[]}`), "k", "")
		ids = append(ids, rec.Header().Get("X-Calvoproxy-Decision-Id"))
	}

	for _, id := range ids[:3] {
		if _, ok := svc.Decision(id); ok {
			t.Errorf("decision %s should have rotated out of a ring of 3", id)
		}
	}
	for _, id := range ids[3:] {
		if _, ok := svc.Decision(id); !ok {
			t.Errorf("decision %s should still be in the ring", id)
		}
	}
}
