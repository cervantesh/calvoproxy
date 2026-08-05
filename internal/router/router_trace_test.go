package router

import (
	"context"
	"net/http"
	"strings"
	"testing"
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
