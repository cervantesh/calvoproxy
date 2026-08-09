package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	// Exactly "2", not merely non-empty: AttemptIndex is 1-based, so a
	// degradation signal hard-wired to "1" would satisfy a non-empty check while
	// reporting every fallback as a first-choice answer.
	if got := rec.Header().Get("X-Calvoproxy-Attempt"); got != "2" {
		t.Errorf("X-Calvoproxy-Attempt = %q, want \"2\" — this is the degraded signal", got)
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
	// A parseable, positive, sane value — not just a present header. "0" or
	// garbage tells a client nothing and it retries immediately anyway, which is
	// the outage amplification this header exists to prevent.
	raw := rec.Header().Get("Retry-After")
	secs, err := strconv.Atoi(raw)
	if err != nil {
		t.Errorf("Retry-After = %q, not an integer number of seconds", raw)
	} else if secs < 1 || secs > 3600 {
		t.Errorf("Retry-After = %ds, outside a usable band; the configured cooldown is 90s", secs)
	}
	if upstream.calls != 0 {
		t.Errorf("an open breaker must not reach upstream, got %d calls", upstream.calls)
	}
}

func TestDispatch_AllModelsCoolingDownPreservesDailyFreeQuotaReason(t *testing.T) {
	upstream := &recordingTransport{}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": {"model-a", "model-b"}},
		Aliases:        map[string]string{"default": "coding", "coding": "coding"},
	})
	svc.config.FailureThreshold = 1
	quotaMessage, ok := openRouterDailyFreeQuotaMessage(realDailyFreeQuota429)
	if !ok {
		t.Fatal("premise: captured daily quota response was not recognized")
	}
	for _, model := range []string{"model-a", "model-b"} {
		svc.recordFailure(modelAttempt{Profile: "coding", Model: model}, http.StatusServiceUnavailable, quotaMessage)
	}

	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[]}`), "k", "")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("all-open chain should be 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "OpenRouter daily free-model quota exhausted") {
		t.Fatalf("cooling response lost the actionable quota reason: %s", rec.Body.String())
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
		t.Fatalf("the opt-in must actually let the request through, got %d calls", upstream.calls)
	}
	// And the client must get the upstream's answer. Counting the call only
	// proves we spent the money, not that anyone got anything for it.
	if rec.Code != http.StatusOK {
		t.Errorf("opted-in embeddings returned %d: %s", rec.Code, rec.Body.String())
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

// statusThenOKTransport answers the first attempt with a chosen status and body,
// then 200. It exists because sequenceTransport cannot carry a body, and the
// body is what distinguishes "this provider is picky" from "this request is
// broken".
type statusThenOKTransport struct {
	first  int
	body   string
	calls  int
	models []string
}

func (t *statusThenOKTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		m, _ := parsed["model"].(string)
		t.models = append(t.models, m)
	}
	status, body := http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`
	if t.calls == 1 {
		status, body = t.first, t.body
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// The incident this whole branch came from, pinned at the level where the
// decision is actually made.
//
// A provider returned 400 "at most 64 tools are allowed". 400 is normally
// terminal, so the chain stopped on the first picky provider and every agent
// turn died in ~0.8s — even though every other model in the chain would have
// answered. The fix makes 400 advance.
//
// Table-driven over the status classes because "which statuses advance" is the
// question that keeps producing incidents, and answering it one status at a
// time is how the 400 case got missed.
func TestUpstreamStatus_AdvancesOrTerminatesTheChain(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCalls  int // 2 = advanced to the second model, 1 = stopped
		wantClient int
	}{
		{
			// The regression guard. If SkipModel is dropped for 400, this fails.
			name:   "400 from a picky provider advances",
			status: http.StatusBadRequest,
			body: `{"error":{"message":"Provider returned error","code":400,` +
				`"metadata":{"raw":"at most 64 tools are allowed"}}}`,
			wantCalls: 2, wantClient: http.StatusOK,
		},
		{
			// CURRENT BEHAVIOUR, recorded rather than endorsed: 402 is terminal.
			//
			// This looks like the same bug class as the 400 above, and this test
			// is how it was found. A proxy whose whole premise is free models
			// gets a 402 when ONE model stops being free -- the rest of the chain
			// is unaffected, so terminating loses a request the next model would
			// have served. The counter-argument is an account-wide credit
			// exhaustion, where advancing burns the chain for nothing.
			//
			// Left unchanged here on purpose: changing routing semantics does not
			// belong in a testing change. If it is made to advance, flip
			// wantCalls to 2 and wantClient to 200 -- and the fact that this line
			// has to change is the point of writing it down.
			name: "402 currently stops the chain (see comment)", status: http.StatusPaymentRequired,
			body:      `{"error":"model requires credit"}`,
			wantCalls: 1, wantClient: http.StatusPaymentRequired,
		},
		{
			name: "429 advances", status: http.StatusTooManyRequests, body: `{"error":"rate limited"}`,
			wantCalls: 2, wantClient: http.StatusOK,
		},
		{
			name: "502 advances", status: http.StatusBadGateway, body: `upstream unavailable`,
			wantCalls: 2, wantClient: http.StatusOK,
		},
		{
			// A bad key is bad for every model in the chain. Advancing would burn
			// the whole chain on a problem no other model can fix, and would hide
			// the one error the operator needs to see.
			name: "401 stops the chain", status: http.StatusUnauthorized, body: `{"error":"invalid key"}`,
			wantCalls: 1, wantClient: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &statusThenOKTransport{first: tc.status, body: tc.body}
			svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
				DefaultProfile: "simple",
				Profiles:       map[string][]string{"simple": {"picky-model", "tolerant-model"}},
				Aliases:        map[string]string{"default": "simple", "simple": "simple"},
			})

			rec := httptest.NewRecorder()
			svc.RouteRequest(rec, trustedRequest(http.MethodPost, "/v1/chat/completions",
				`{"messages":[{"role":"user","content":"hi"}]}`), "k")

			if upstream.calls != tc.wantCalls {
				t.Errorf("upstream called %d times, want %d (models tried: %v)",
					upstream.calls, tc.wantCalls, upstream.models)
			}
			if rec.Code != tc.wantClient {
				t.Errorf("client got %d, want %d: %s", rec.Code, tc.wantClient, rec.Body.String())
			}
			if tc.wantCalls == 2 && len(upstream.models) == 2 && upstream.models[1] != "tolerant-model" {
				t.Errorf("advanced to %q, want tolerant-model", upstream.models[1])
			}
		})
	}
}
