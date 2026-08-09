package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Twenty-six of 183 requests on a live instance failed with nothing recording
// why. These tests pin the classification, and in particular the one case that
// a naive implementation gets backwards.

func newChainTestService(t *testing.T, client HTTPDoer, models []string) *RouterService {
	t.Helper()
	return newTestService(t, client, policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": models},
	})
}

// disconnectMidFlightTransport hangs the client up during the first upstream
// call and then fails every call with the context error, the way the real
// transport does once the request context is cancelled.
type disconnectMidFlightTransport struct {
	cancel context.CancelFunc
	calls  int
}

func (t *disconnectMidFlightTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	if t.calls == 1 && t.cancel != nil {
		t.cancel()
	}
	return nil, context.Canceled
}

// THE test for this change. A client hanging up must be recorded as
// "cancelled", never as "exhausted".
//
// The trap it guards: executeAttempt converts a cancelled parent context into
// attemptError{Retryable:false, SkipModel:true}, and the fallback loop treats
// SkipModel as `continue`. So a disconnect does NOT stop the chain — it burns
// every remaining model, one cancelled attempt at a time, and leaves through
// the normal end of the loop. Any implementation that maps "which exit fired"
// or "what shape was lastErr" to a reason reports this as exhausted (or
// terminal, since Retryable is false) and blames the models for the client.
func TestChainFailure_ClientCancelIsRecordedAsCancelledNotExhausted(t *testing.T) {
	// The disconnect must land MID-FLIGHT, not before the request starts: a
	// context that is already dead is refused by policy authorization and never
	// reaches the chain at all, which is a different (and much easier) path.
	transport := &disconnectMidFlightTransport{}
	svc := newChainTestService(t, &http.Client{Transport: transport},
		[]string{"a/one:free", "a/two:free", "a/three:free"})

	req := trustedRequest(http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	transport.cancel = cancel // the client hangs up during the first attempt
	svc.RouteRequestWithProvider(httptest.NewRecorder(), req.WithContext(ctx), "k", string(providerOpenRouter))

	c := svc.Counters()
	if c.ChainFailedCancelled != 1 {
		t.Errorf("a client disconnect must be counted as cancelled, got %d", c.ChainFailedCancelled)
	}
	if c.ChainFailedExhausted != 0 {
		t.Errorf("a disconnect blamed on the models: exhausted=%d", c.ChainFailedExhausted)
	}
	if c.ChainFailedTerminal != 0 {
		t.Errorf("Retryable:false read as terminal: terminal=%d", c.ChainFailedTerminal)
	}
	// The premise of the trap, pinned so the test keeps testing what it claims:
	// the cancel really does run the chain to the end rather than stopping it.
	// If this ever becomes 1, the classification above stops being the hard case
	// and this test silently weakens.
	if transport.calls < 2 {
		t.Errorf("premise changed: a cancel now stops the chain after %d attempt(s); "+
			"re-check that classifyChainFailure is still being exercised on the hard path", transport.calls)
	}
}

// A whole-chain deadline is a different diagnosis from a disconnect, and has
// the same error shape, so only ctx.Err() can separate them.
func TestChainFailure_TotalTimeoutIsDistinctFromCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	// The loop's own verdict says "exhausted"; the context must override it.
	err := &chainError{reason: chainExhausted, err: errors.New("boom")}
	if got := classifyChainFailure(ctx, err); got != chainTotalTimeout {
		t.Errorf("expired total budget classified as %q, want total_timeout", got)
	}
}

// A non-retryable error with models still untried: the chain stopped early and
// spent options it never used. That is the actionable case.
func TestChainFailure_TerminalWhenModelsRemainUntried(t *testing.T) {
	executor := &fakeAttemptExecutor{errs: []error{
		&attemptError{StatusCode: http.StatusUnauthorized, Message: "bad key"},
	}}
	err := DefaultFallbackExecutor{AttemptExecutor: executor}.Execute(
		context.Background(), httptest.NewRecorder(), FallbackExecution{
			RequestBody: map[string]interface{}{},
			Attempts: []modelAttempt{
				{Profile: "coding", Model: "a/one:free"},
				{Profile: "coding", Model: "a/two:free"},
				{Profile: "coding", Model: "a/three:free"},
			},
		})

	if len(executor.attempts) != 1 {
		t.Fatalf("premise: a terminal error must stop the chain, got %d attempts", len(executor.attempts))
	}
	if got := classifyChainFailure(context.Background(), err); got != chainTerminal {
		t.Errorf("got %q, want terminal", got)
	}
}

// The mirror case, and the reason the flag is not simply "did we break": a
// non-retryable error on the LAST model breaks with nothing left untried. The
// chain got its full run, so that is exhausted — counting it as terminal would
// report "stopped early" for a chain that stopped at the end.
func TestChainFailure_BreakOnLastModelIsExhaustedNotTerminal(t *testing.T) {
	executor := &fakeAttemptExecutor{errs: []error{
		&attemptError{StatusCode: http.StatusBadGateway, Retryable: true, Message: "gateway"},
		&attemptError{StatusCode: http.StatusUnauthorized, Message: "bad key"},
	}}
	err := DefaultFallbackExecutor{AttemptExecutor: executor}.Execute(
		context.Background(), httptest.NewRecorder(), FallbackExecution{
			RequestBody: map[string]interface{}{},
			RetryPolicy: RetryPolicy{RetryHTTPStatuses: []int{http.StatusBadGateway}},
			Attempts: []modelAttempt{
				{Profile: "coding", Model: "a/one:free"},
				{Profile: "coding", Model: "a/two:free"},
			},
		})

	if len(executor.attempts) != 2 {
		t.Fatalf("premise: both models should have been tried, got %d", len(executor.attempts))
	}
	if got := classifyChainFailure(context.Background(), err); got != chainExhausted {
		t.Errorf("a break on the last model classified as %q, want exhausted", got)
	}
}

// Every model tried, every one failed, loop ended normally.
func TestChainFailure_ExhaustedWhenEveryModelFailed(t *testing.T) {
	executor := &fakeAttemptExecutor{errs: []error{
		&attemptError{StatusCode: http.StatusBadGateway, Retryable: true, Message: "one"},
		&attemptError{StatusCode: http.StatusBadGateway, Retryable: true, Message: "two"},
	}}
	err := DefaultFallbackExecutor{AttemptExecutor: executor}.Execute(
		context.Background(), httptest.NewRecorder(), FallbackExecution{
			RequestBody: map[string]interface{}{},
			RetryPolicy: RetryPolicy{RetryHTTPStatuses: []int{http.StatusBadGateway}},
			Attempts: []modelAttempt{
				{Profile: "coding", Model: "a/one:free"},
				{Profile: "coding", Model: "a/two:free"},
			},
		})

	if got := classifyChainFailure(context.Background(), err); got != chainExhausted {
		t.Errorf("got %q, want exhausted", got)
	}
}

func TestChainFailure_ExecutorErrorIsItsOwnReason(t *testing.T) {
	err := DefaultFallbackExecutor{}.Execute(context.Background(), httptest.NewRecorder(), FallbackExecution{})
	if got := classifyChainFailure(context.Background(), err); got != chainExecutorError {
		t.Errorf("an unconfigured executor classified as %q, want executor_error", got)
	}
}

// fallbackErrorResponse reads the attemptError out of the returned error to
// choose the client's status code. Wrapping that error to carry the reason must
// not break it — otherwise every chain failure becomes a flat 502 and the
// client loses the upstream's actual status.
func TestChainError_PreservesClientVisibleStatusAndMessage(t *testing.T) {
	executor := &fakeAttemptExecutor{errs: []error{
		&attemptError{StatusCode: http.StatusUnauthorized, Message: "invalid API key"},
	}}
	err := DefaultFallbackExecutor{AttemptExecutor: executor}.Execute(
		context.Background(), httptest.NewRecorder(), FallbackExecution{
			RequestBody: map[string]interface{}{},
			Attempts:    []modelAttempt{{Profile: "coding", Model: "a/one:free"}},
		})

	status, message := fallbackErrorResponse(err)
	if status != http.StatusUnauthorized {
		t.Errorf("status %d lost through the reason wrapper, want 401", status)
	}
	if message != "invalid API key" {
		t.Errorf("message %q lost through the reason wrapper", message)
	}
}

// The 503 that never had a counter: every planned model open or cooling down.
// It is refused BEFORE executeFallbacks runs, so it must not appear as a chain
// failure — the chain never ran.
func TestAllModelsCoolingIsCountedAndIsNotAChainFailure(t *testing.T) {
	transport := &streamTransport{events: "data: {}\n\n"}
	svc := newChainTestService(t, &http.Client{Transport: transport}, []string{"a/one:free", "a/two:free"})

	// Open every circuit in the profile.
	for _, model := range []string{"a/one:free", "a/two:free"} {
		attempt := modelAttempt{Profile: "coding", Model: model,
			BreakerPolicy: BreakerPolicy{FailureThreshold: 1, Cooldown: time.Minute, Eligible: true}}
		svc.recordFailure(attempt, http.StatusBadGateway, "down")
	}

	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`), "k", string(providerOpenRouter))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("premise: expected the cooling-down 503, got %d", rec.Code)
	}
	c := svc.Counters()
	if c.AllModelsCooling != 1 {
		t.Errorf("the cooling-down 503 was not counted: %d", c.AllModelsCooling)
	}
	if total := c.ChainFailedCancelled + c.ChainFailedTotalTimeout + c.ChainFailedTerminal +
		c.ChainFailedExhausted + c.ChainFailedExecutorError; total != 0 {
		t.Errorf("a chain that never ran was counted as a chain failure (%d)", total)
	}
	if transport.calls != 0 {
		t.Errorf("premise: no upstream call should have been made, got %d", transport.calls)
	}
}

// --- Part B: per-model time-to-first-event -------------------------------

// queuedStreamTransport answers 200 text/event-stream immediately — the model
// "accepted" — and then sits on keepalive comments, which is exactly how a
// queued upstream behaves and exactly what a Do-level stopwatch cannot see.
type queuedStreamTransport struct {
	body io.ReadCloser
}

func (t *queuedStreamTransport) RoundTrip(*http.Request) (*http.Response, error) {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: t.body}, nil
}

func firstEventSample(t *testing.T, svc *RouterService, model string) ModelLatency {
	t.Helper()
	for _, m := range svc.Counters().FirstEventLatency {
		if m.Model == "coding:"+model {
			return m
		}
	}
	t.Fatalf("no first-event sample recorded for %q", model)
	return ModelLatency{}
}

// Recorded when the event ARRIVES, not only when the budget expires. Timeouts
// alone would make every sample equal the budget, hiding the healthy
// 0.35-0.70s population that decides whether the budget is tuned correctly.
func TestFirstEventLatency_RecordedWhenTheEventArrives(t *testing.T) {
	t.Setenv("PROXY_STREAM_FIRST_BYTE_TIMEOUT", "5")
	svc := newChainTestService(t, &http.Client{Transport: &queuedStreamTransport{
		body: io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\ndata: [DONE]\n\n")),
	}}, []string{"a/one:free"})

	attempt := modelAttempt{Profile: "coding", Model: "a/one:free"} // not LastInChain
	if err := svc.executeAttempt(context.Background(), httptest.NewRecorder(), []byte(`{}`), "k", attempt); err != nil {
		t.Fatalf("premise: the stream should have succeeded: %v", err)
	}

	got := firstEventSample(t, svc, "a/one:free")
	if got.Count != 1 {
		t.Errorf("successful first-event waits are not being sampled: count=%d", got.Count)
	}
	if svc.Counters().StreamFirstEventTimeout != 0 {
		t.Error("premise: this attempt did not time out")
	}
}

// And on the timeout path too, so the tail is visible next to the healthy body
// of the distribution rather than being the only thing recorded.
func TestFirstEventLatency_RecordedWhenTheBudgetExpires(t *testing.T) {
	t.Setenv("PROXY_STREAM_FIRST_BYTE_TIMEOUT", "1")
	svc := newChainTestService(t, &http.Client{Transport: &queuedStreamTransport{
		body: &slowBody{chunks: []string{": OPENROUTER PROCESSING\n"}, delay: 3 * time.Second},
	}}, []string{"a/one:free"})

	attempt := modelAttempt{Profile: "coding", Model: "a/one:free"}
	err := svc.executeAttempt(context.Background(), httptest.NewRecorder(), []byte(`{}`), "k", attempt)
	if err == nil {
		t.Fatal("premise: the attempt should have been abandoned")
	}

	got := firstEventSample(t, svc, "a/one:free")
	if got.Count != 1 {
		t.Errorf("abandoned waits are not being sampled: count=%d", got.Count)
	}
	if got.SumSeconds < 0.5 {
		t.Errorf("the recorded wait (%.3fs) does not reflect the ~1s budget actually spent", got.SumSeconds)
	}
}

// headerDelayTransport reproduces what a live upstream actually did: hold the
// response headers, then flush the queue keepalives and the first data event
// together. The post-header wait is then ~0 even though time-to-first-token was
// seconds — measured on OpenRouter, 1.51s TTFT recorded as under 5ms of
// post-header wait. This is the whole reason first_token exists next to
// first_event.
type headerDelayTransport struct {
	delay time.Duration
}

func (t *headerDelayTransport) RoundTrip(*http.Request) (*http.Response, error) {
	time.Sleep(t.delay) // the upstream sits on the headers
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	return &http.Response{StatusCode: http.StatusOK, Header: h,
		Body: io.NopCloser(strings.NewReader(": OPENROUTER PROCESSING\n\ndata: {}\n\n"))}, nil
}

func firstTokenSample(t *testing.T, svc *RouterService, model string) ModelLatency {
	t.Helper()
	for _, m := range svc.Counters().FirstTokenLatency {
		if m.Model == "coding:"+model {
			return m
		}
	}
	t.Fatalf("no first-token sample recorded for %q", model)
	return ModelLatency{}
}

// The defect that running this against a real upstream exposed: measuring only
// the post-header wait reports a slow model as instant, because the delay
// arrives before the headers do. first_token must include it.
func TestFirstTokenLatency_IncludesTheWaitForResponseHeaders(t *testing.T) {
	t.Setenv("PROXY_STREAM_FIRST_BYTE_TIMEOUT", "5")
	svc := newChainTestService(t, &http.Client{Transport: &headerDelayTransport{delay: 300 * time.Millisecond}},
		[]string{"a/one:free"})

	attempt := modelAttempt{Profile: "coding", Model: "a/one:free"}
	if err := svc.executeAttempt(context.Background(), httptest.NewRecorder(), []byte(`{}`), "k", attempt); err != nil {
		t.Fatalf("premise: the stream should have succeeded: %v", err)
	}

	token := firstTokenSample(t, svc, "a/one:free")
	if token.Count != 1 {
		t.Fatalf("first-token samples not recorded: count=%d", token.Count)
	}
	if token.SumSeconds < 0.25 {
		t.Errorf("first-token %.3fs excludes the 0.3s header wait — this is the exact blind spot the metric exists to close", token.SumSeconds)
	}

	// And the two series must stay distinct: the post-header wait saw almost
	// none of that delay. If these ever converge, one of them is measuring the
	// wrong thing and summing them would be meaningless.
	event := firstEventSample(t, svc, "a/one:free")
	if event.SumSeconds > 0.15 {
		t.Errorf("post-header wait %.3fs unexpectedly absorbed the header delay", event.SumSeconds)
	}
	if token.SumSeconds <= event.SumSeconds {
		t.Errorf("first_token (%.3fs) must exceed first_event (%.3fs): it starts earlier", token.SumSeconds, event.SumSeconds)
	}
}

// An abandoned attempt never produced a token. Averaging "how long we waited
// before giving up" into first_token would drag the number toward the budget
// and make a model look slower the more often it was abandoned — the same
// backwards-ranking bug in a new place. The abandonment is counted separately.
func TestFirstTokenLatency_AbandonedAttemptContributesNoSample(t *testing.T) {
	t.Setenv("PROXY_STREAM_FIRST_BYTE_TIMEOUT", "1")
	svc := newChainTestService(t, &http.Client{Transport: &queuedStreamTransport{
		body: &slowBody{chunks: []string{": OPENROUTER PROCESSING\n"}, delay: 3 * time.Second},
	}}, []string{"a/one:free"})

	attempt := modelAttempt{Profile: "coding", Model: "a/one:free"}
	if err := svc.executeAttempt(context.Background(), httptest.NewRecorder(), []byte(`{}`), "k", attempt); err == nil {
		t.Fatal("premise: the attempt should have been abandoned")
	}

	if got := svc.Counters().FirstTokenLatency; len(got) != 0 {
		t.Errorf("an attempt that produced no token contributed a first-token sample: %+v", got)
	}
	// The abandonment is not lost — it is counted where it belongs.
	if svc.Counters().StreamFirstEventTimeout != 1 {
		t.Error("the abandonment should still be counted as a first-event timeout")
	}
	// And the post-header wait DOES sample it, since that series exists to show
	// what the budget acted on.
	if got := firstEventSample(t, svc, "a/one:free"); got.Count != 1 {
		t.Errorf("first_event must still sample the abandoned wait: count=%d", got.Count)
	}
}

func TestFirstEventTimeoutContinuesCurrentStreamWhenFallbackCannotBeReserved(t *testing.T) {
	t.Setenv("PROXY_STREAM_FIRST_BYTE_TIMEOUT", "1")
	svc := newChainTestService(t, &http.Client{Transport: &queuedStreamTransport{
		body: &slowBody{chunks: []string{"data: {\"choices\":[]}\n\ndata: [DONE]\n\n"}, delay: 1100 * time.Millisecond},
	}}, []string{"a/one:free"})

	reservationCalls := 0
	attempt := modelAttempt{
		Profile: "coding",
		Model:   "a/one:free",
		ReserveFallback: func() bool {
			reservationCalls++
			return false
		},
	}
	if err := svc.executeAttempt(context.Background(), httptest.NewRecorder(), []byte(`{}`), "k", attempt); err != nil {
		t.Fatalf("current stream should continue when no fallback quota can be claimed: %v", err)
	}
	if reservationCalls != 1 {
		t.Fatalf("expected one just-in-time fallback reservation attempt, got %d", reservationCalls)
	}
	if svc.Counters().StreamFirstEventTimeout != 0 {
		t.Fatal("a protected current stream must not be counted as abandoned")
	}
}

// The label space must match calvoproxy_model_score's, so the two metrics join
// on the same key and cardinality stays bounded by the policy.
func TestFirstEventLatency_UsesTheModelScoreKeySpace(t *testing.T) {
	t.Setenv("PROXY_STREAM_FIRST_BYTE_TIMEOUT", "5")
	svc := newChainTestService(t, &http.Client{Transport: &queuedStreamTransport{
		body: io.NopCloser(strings.NewReader("data: {}\n\n")),
	}}, []string{"a/one:free"})

	attempt := modelAttempt{Profile: "coding", Model: "a/one:free"}
	_ = svc.executeAttempt(context.Background(), httptest.NewRecorder(), []byte(`{}`), "k", attempt)

	samples := svc.Counters().FirstEventLatency
	if len(samples) != 1 {
		t.Fatalf("expected one model sampled, got %+v", samples)
	}
	if want := svc.breakerKey(attempt); samples[0].Model != want {
		t.Errorf("label %q diverges from the breaker/score key %q", samples[0].Model, want)
	}
}
