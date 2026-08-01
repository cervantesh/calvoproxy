package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

// Coverage of RouteRequestWithProvider / dispatchChain / streamProxyResponse —
// the three functions every request passes through, and the three that were
// least covered (41.7% / 45.2% / 0%) while the repo shipped 20 releases in 30
// hours. Each test below pins a branch that decides what a *client* sees:
// a status code, a header, or bytes on the wire.

// streamTransport answers with a real text/event-stream response so the
// streaming half of executeAttempt runs for real.
type streamTransport struct {
	events  string
	extra   http.Header
	calls   int
	lastURL string
}

func (t *streamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	t.lastURL = req.URL.String()
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	for k, vs := range t.extra {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(t.events)),
	}, nil
}

func newTestService(t *testing.T, client HTTPDoer, policy policyConfig) *RouterService {
	t.Helper()
	svc := NewRouterService()
	svc.Client = client
	cfg := normalizeRuleConfig(ruleRuntimeConfig{PolicyRuntimeConfig: cervoruntime.PolicyRuntimeConfig{
		OperationTargets: testOperationTargets(),
		TrustedUsers:     []string{"cervantes"},
		DefaultExecutor:  providerOpenRouter,
	}})
	svc.runtimeConfig = cfg
	svc.PolicyEngine = mustBuildPolicyFlow(t, cfg)
	svc.setModelPolicyConfig(policy)
	t.Cleanup(svc.Close)
	return svc
}

// headerSnapshotRecorder exists because httptest.ResponseRecorder does NOT
// snapshot headers at WriteHeader — it hands out one live map, so a header set
// too late still shows up in Header() and an ordering assertion silently passes.
// Verified: moving setServedModelHeaders after streamProxyResponse left the
// plain-recorder test green. This records what a real client would have seen.
type headerSnapshotRecorder struct {
	*httptest.ResponseRecorder
	sent    http.Header
	written bool
}

func newHeaderSnapshotRecorder() *headerSnapshotRecorder {
	return &headerSnapshotRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *headerSnapshotRecorder) WriteHeader(code int) {
	if !r.written {
		r.written = true
		r.sent = r.Header().Clone()
	}
	r.ResponseRecorder.WriteHeader(code)
}

func (r *headerSnapshotRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseRecorder.Write(b)
}

// sentHeader reports a header as the client received it: from the snapshot once
// the response has been committed, otherwise from the pending map.
func (r *headerSnapshotRecorder) sentHeader(key string) string {
	if r.sent != nil {
		return r.sent.Get(key)
	}
	return r.Header().Get(key)
}

func trustedRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Cervo-Capability", string(capChatCompletion))
	req.Header.Set("X-Cervo-User", "cervantes")
	return req
}

// streamProxyResponse had 0% coverage while streaming was the newest thing in
// production. The header order is the load-bearing part: setServedModelHeaders
// must land BEFORE WriteHeader, or the client streams an answer with no way to
// know which model produced it.
func TestStreaming_ServedModelHeadersPrecedeTheBody(t *testing.T) {
	upstream := &streamTransport{
		events: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
		extra:  http.Header{"X-Upstream-Trace": []string{"abc123"}},
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
	// sentHeader, not Header(): the claim is that the header was on the wire
	// BEFORE the first byte, and only the snapshot can tell the difference.
	if got := rec.sentHeader("X-Calvoproxy-Model"); got != "model-a" {
		t.Errorf("served-model header = %q at WriteHeader time; a client streaming a "+
			"response cannot otherwise tell which model answered", got)
	}
	if got := rec.sentHeader("Content-Type"); !strings.Contains(got, "event-stream") {
		t.Errorf("upstream content-type must reach the client, got %q", got)
	}
	if got := rec.sentHeader("X-Upstream-Trace"); got != "abc123" {
		t.Errorf("streamProxyResponse must copy upstream headers, got %q", got)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("stream body not piped through: %q", rec.Body.String())
	}
}

// A degraded answer must announce itself. This is the incident that motivated
// the headers: a design review answered by a later model in the chain, believed
// to be the first.
func TestFallback_MarksTheDegradedAttempt(t *testing.T) {
	upstream := &sequenceTransport{statuses: []int{http.StatusBadGateway, http.StatusOK}}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a", "model-b"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	// RouteRequest, not RouteRequestWithProvider: this is the entry point the
	// server actually calls, so at least one test has to go through it.
	rec := httptest.NewRecorder()
	svc.RouteRequest(rec, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[]}`), "k")

	if rec.Code != http.StatusOK {
		t.Fatalf("chain should recover on model-b, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Calvoproxy-Model"); got != "model-b" {
		t.Errorf("X-Calvoproxy-Model = %q, want model-b", got)
	}
	if got := rec.Header().Get("X-Calvoproxy-Attempt"); got == "" || got == "0" {
		t.Errorf("a fallback answer must carry the attempt index, got %q", got)
	}
}

// Every model cooling down: the client needs to know WHEN to come back.
// Without Retry-After, clients retry immediately and amplify the outage.
func TestDispatch_AllModelsCoolingDownAdvertisesRetryAfter(t *testing.T) {
	upstream := &recordingTransport{}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})
	// Trip the only model in the chain.
	svc.config.FailureThreshold = 1
	svc.config.Cooldown = 90 * time.Second
	svc.recordFailure(modelAttempt{Profile: "simple", Model: "model-a"}, http.StatusTooManyRequests, "rate limited")

	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[]}`), "k", "")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("all-open chain should be 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("503 without Retry-After: clients retry immediately and amplify the outage")
	}
	if upstream.calls != 0 {
		t.Errorf("an open breaker must not reach upstream, got %d calls", upstream.calls)
	}
}

// A caller who pins a model that cannot see images gets a clear 422 instead of
// a silent upstream breakage. 422 is deliberate: it is not retryable, unlike
// the 503 the chain returns when NO model qualifies.
func TestDispatch_PinnedModelMissingCapabilityIs422(t *testing.T) {
	upstream := &recordingTransport{}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"text-only"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})
	svc.capabilities = newCapabilityIndex(map[string][]string{"text-only": {"tools"}})

	body := `{"model":"text-only","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(http.MethodPost, "/v1/chat/completions", body), "k", "")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("pinned model without vision should be 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstream.calls != 0 {
		t.Errorf("must not reach upstream, got %d calls", upstream.calls)
	}
}

// No model in ANY profile can serve the request: a 503 distinct from the
// breaker one, so an operator can tell "everything is cooling down" from
// "nothing here can do vision".
func TestDispatch_NoCapableModelAnywhereIs503(t *testing.T) {
	upstream := &recordingTransport{}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"text-only"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})
	svc.capabilities = newCapabilityIndex(map[string][]string{"text-only": {}})

	body := `{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(http.MethodPost, "/v1/chat/completions", body), "k", "")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no capable model should be 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "supports") {
		t.Errorf("the 503 must say it is a capability problem, not a cooldown: %s", rec.Body.String())
	}
	if upstream.calls != 0 {
		t.Errorf("must not reach upstream, got %d calls", upstream.calls)
	}
}

// /v1/messages runs the SAME chain, breaker and fallback as chat — that is the
// whole claim of the Anthropic path, and nothing tested it end to end.
func TestMessagesPath_UsesTheModelChainAndTargetsMessagesUpstream(t *testing.T) {
	upstream := &sequenceTransport{statuses: []int{http.StatusBadGateway, http.StatusOK}}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a", "model-b"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/messages", `{"messages":[{"role":"user","content":"hi"}]}`), "k", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("messages should fall back like chat, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(upstream.urls) != 2 {
		t.Fatalf("expected the chain to advance once, got %v", upstream.urls)
	}
	for _, u := range upstream.urls {
		if !strings.Contains(u, "messages") {
			t.Errorf("messages attempts must target the messages endpoint, got %q", u)
		}
	}
}

// An unroutable /messages body still has to be authorized and then tunnelled,
// not 400'd — some Anthropic clients send shapes this proxy does not model.
func TestMessagesPath_UnroutableBodyTunnels(t *testing.T) {
	upstream := &recordingTransport{}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/messages", `not json at all`), "k", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("unroutable messages body should tunnel, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstream.calls != 1 {
		t.Fatalf("expected one passthrough call, got %d", upstream.calls)
	}
}

// A chat body that is not JSON is a client error and must never reach upstream.
func TestRoute_InvalidJSONIs400AndNeverReachesUpstream(t *testing.T) {
	upstream := &recordingTransport{}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":`), "k", "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON should be 400, got %d", rec.Code)
	}
	if upstream.calls != 0 {
		t.Errorf("must not reach upstream, got %d calls", upstream.calls)
	}
}

// The embeddings guard is a spend boundary: an account that never opted in
// cannot be billed by a path it did not know existed.
func TestEmbeddings_RefusedByDefaultAndAllowedOnOptIn(t *testing.T) {
	upstream := &recordingTransport{}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/embeddings", `{"input":"hi"}`), "k", "")

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("embeddings must be refused by default, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstream.calls != 0 {
		t.Fatalf("a refused embedding must not spend, got %d calls", upstream.calls)
	}

	t.Setenv("PROXY_ALLOW_PAID_EMBEDDINGS", "true")
	rec = httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/embeddings", `{"input":"hi"}`), "k", "")

	if upstream.calls != 1 {
		t.Errorf("the opt-in must actually let the request through, got %d calls", upstream.calls)
	}
}

// The body cap exists so a hostile payload cannot OOM the process. It must fire
// before the body is read into memory, hence 413 with no upstream call.
func TestRoute_OversizedBodyIs413(t *testing.T) {
	upstream := &recordingTransport{}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})
	t.Setenv("PROXY_MAX_BODY_BYTES", "64")

	huge := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", 4096) + `"}]}`
	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(http.MethodPost, "/v1/chat/completions", huge), "k", "")

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body should be 413, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstream.calls != 0 {
		t.Errorf("must not reach upstream, got %d calls", upstream.calls)
	}
}
