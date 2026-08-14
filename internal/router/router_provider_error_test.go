package router

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Bodies captured from production, not invented. Both came from the same
// provider on an account whose own key was valid at the time.
const (
	realToolCap400 = `{"error":{"message":"Provider returned error","code":400,` +
		`"metadata":{"raw":"{\"error\":{\"code\":\"invalid_request_error\",\"message\":\"at most 64 tools are allowed\"}}\n",` +
		`"provider_name":"Darkbloom","is_byok":false}}}`

	realProviderAuth401 = `{"error":{"message":"Provider returned error","code":401,` +
		`"metadata":{"raw":"{\"error\":{\"code\":\"authentication_error\",\"message\":\"invalid API key\",\"type\":\"authentication_error\"}}\n",` +
		`"provider_name":"Darkbloom","is_byok":false}}}`

	// OpenRouter refusing the caller's own key looks nothing like the above:
	// no "Provider returned error", no provider_name.
	realAccountAuth401 = `{"error":{"message":"No auth credentials found","code":401}}`

	// Groq refusing an ordinary agent request outright: the account's
	// tokens-per-minute window is smaller than a single call with tool
	// schemas attached, so no amount of waiting or retrying helps — only a
	// provider with a larger window can serve it. Captured 2026-08-14 from a
	// daily cron job that had failed on this every run.
	realGroqTPM413 = `{"error":{"message":"Request too large for model ` + "`openai/gpt-oss-120b`" +
		` in organization ` + "`org_01km2d6qfwegmt0pkr9y55xb7b`" + ` service tier ` + "`on_demand`" +
		` on tokens per minute (TPM): Limit 8000, Requested 18711, please reduce your message size and try again.",` +
		`"type":"tokens","code":"rate_limit_exceeded"}}`

	realDailyFreeQuota429 = `{"error":{"message":"Rate limit exceeded: free-models-per-day-high-balance. ","code":429,` +
		`"metadata":{"headers":{"X-RateLimit-Limit":"1000","X-RateLimit-Remaining":"0",` +
		`"X-RateLimit-Reset":"1786233600000"},"limit_source":"openrouter_free_tier_daily",` +
		`"remedy_hint":"Wait for the daily reset"}}}`
)

func TestOpenRouterDailyFreeQuotaMessage_Actionable(t *testing.T) {
	message, ok := openRouterDailyFreeQuotaMessage(realDailyFreeQuota429)
	if !ok {
		t.Fatal("expected the captured OpenRouter daily free quota error to be recognized")
	}
	for _, want := range []string{
		"1000/1000",
		"2026-08-09 00:00 UTC",
		"another :free model will not help",
		"direct provider/paid model",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not contain %q", message, want)
		}
	}
}

func TestOpenRouterDailyFreeQuotaMessage_DoesNotRewriteOther429s(t *testing.T) {
	if message, ok := openRouterDailyFreeQuotaMessage(`{"error":{"message":"rate limited","code":429}}`); ok || message != "" {
		t.Fatalf("generic 429 must keep the normal classifier message, got ok=%v message=%q", ok, message)
	}
}

type dailyFreeQuotaTransport struct {
	calls int
}

func (t *dailyFreeQuotaTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(realDailyFreeQuota429)),
	}, nil
}

func TestOpenRouterDailyFreeQuotaIsRedactedAfterChainFailure(t *testing.T) {
	upstream := &dailyFreeQuotaTransport{}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": {"first:free", "second:free"}},
		Aliases:        map[string]string{"default": "coding", "coding": "coding"},
	})

	rec := newHeaderSnapshotRecorder()
	svc.RouteRequest(rec, trustedRequest(http.MethodPost, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"hi"}]}`), "k")

	if upstream.calls != 1 {
		t.Fatalf("account-wide quota should stop sibling OpenRouter attempts, got %d calls", upstream.calls)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected client-visible 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("client body must expose only the safe classification, got %q", rec.Body.String())
	}
	for _, forbidden := range []string{"1000/1000", "another :free model", "OpenRouter daily"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("client body leaked provider detail %q: %q", forbidden, rec.Body.String())
		}
	}
}

func TestProviderRelayedError_Recognition(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"tool-cap 400 relayed from a provider", realToolCap400, true},
		{"auth 401 relayed from a provider", realProviderAuth401, true},
		{"OpenRouter rejecting our own key", realAccountAuth401, false},
		{"empty body", "", false},
		{"unrelated error", `{"error":{"message":"rate limited","code":429}}`, false},
		{
			// The marker string alone is not enough: without provider_name
			// there is nobody to attribute the failure to.
			name: "marker without provider_name",
			body: `{"error":{"message":"Provider returned error","code":500,"metadata":{}}}`,
			want: false,
		},
		{
			// A body that merely mentions the phrase must not flip the chain.
			name: "phrase inside a message, not the marker",
			body: `{"error":{"message":"the upstream said Provider returned error once","code":400}}`,
			want: false,
		},
		{"not json but contains the marker", `Provider returned error`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProviderRelayedError(tc.body); got != tc.want {
				t.Errorf("isProviderRelayedError = %v, want %v", got, tc.want)
			}
		})
	}
}

// The behaviour that matters: a provider-relayed failure advances the chain
// whatever its status, and an account-level refusal still terminates.
func TestProviderRelayedError_ChainBehaviour(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantCalls  int // 2 = advanced to the second model, 1 = stopped
		wantClient int
	}{
		{
			// The 2026-08-03 incident. Was 1 call and a dead turn.
			name:   "401 relayed from a provider advances",
			status: http.StatusUnauthorized, body: realProviderAuth401,
			wantCalls: 2, wantClient: http.StatusOK,
		},
		{
			name:   "400 relayed from a provider advances", // the 64-tool case
			status: http.StatusBadRequest, body: realToolCap400,
			wantCalls: 2, wantClient: http.StatusOK,
		},
		{
			// The 2026-08-14 cron incident. A provider whose per-minute token
			// window (8k) is smaller than one agent request (18.7k) can never
			// serve it, but the next model in the chain has a window several
			// times larger. This was terminal, so a daily job failed every run.
			name:   "413 from a provider's token ceiling advances",
			status: http.StatusRequestEntityTooLarge, body: realGroqTPM413,
			wantCalls: 2, wantClient: http.StatusOK,
		},
		{
			// A bad account key is bad for every model in the chain. Advancing
			// would burn K attempts and bury the one error worth reading.
			name:   "401 from OpenRouter itself still stops",
			status: http.StatusUnauthorized, body: realAccountAuth401,
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

			rec := newHeaderSnapshotRecorder()
			svc.RouteRequest(rec, trustedRequest(http.MethodPost, "/v1/chat/completions",
				`{"messages":[{"role":"user","content":"hi"}]}`), "k")

			if upstream.calls != tc.wantCalls {
				t.Errorf("upstream called %d times, want %d (models: %v)",
					upstream.calls, tc.wantCalls, upstream.models)
			}
			if rec.Code != tc.wantClient {
				t.Errorf("client got %d, want %d: %s", rec.Code, tc.wantClient, rec.Body.String())
			}
		})
	}
}
