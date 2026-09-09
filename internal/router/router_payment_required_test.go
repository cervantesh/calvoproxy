package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type paymentRequiredTransport struct{}

func (paymentRequiredTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusPaymentRequired,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"upstream unavailable"}`)),
	}, nil
}

// A provider that says "Payment Required" is making a statement about ITS OWN
// billing account, never about the other providers in the chain — those
// authenticate with different credentials and are billed separately.
//
// Observed 2026-09-09: Cerebras returned 402 on two scheduled runs. 402 was not
// in the providerAuthFailure set, so it stayed terminal and the chain died on
// attempt 2 of 15, leaving Groq and twelve OpenRouter models untried. The user
// got "HTTP 402: upstream unavailable" for both cron jobs; every one of those
// thirteen models answered fine when probed minutes later.
func TestDirectProviderPaymentRequiredLeavesTheRestOfTheChainAvailable(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "unpaid-secret")
	s := NewRouterService()
	t.Cleanup(s.Close)
	s.Client = &http.Client{Transport: paymentRequiredTransport{}}
	attempt := modelAttempt{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b", BreakerPolicy: BreakerPolicy{Eligible: true}}
	ctx := WithAmbientProviderCredentials(context.Background(), true)

	err := s.executeAttempt(ctx, httptest.NewRecorder(), []byte(`{"messages":[]}`), "", attempt)

	attErr, ok := err.(*attemptError)
	if !ok {
		t.Fatalf("expected an *attemptError, got %T (%v)", err, err)
	}
	if attErr.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected the 402 to be preserved, got %d", attErr.StatusCode)
	}
	// ProviderUnavailable is what makes the fallback loop skip this provider's
	// siblings and keep going. Without it the loop reaches shouldRetryAttempt,
	// finds 402 non-retryable, and breaks out of the whole chain.
	if !attErr.ProviderUnavailable {
		t.Fatal("a direct provider's 402 must exclude that provider only, not end the chain")
	}
}

// The chain-level statement of the same property, independent of how the
// attempt error is spelled: models on other providers still get their turn.
func TestChainContinuesPastAProviderThatDemandsPayment(t *testing.T) {
	attempts := []modelAttempt{
		{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b"},
		{Profile: "coding", Provider: providerGroq, Model: "openai/gpt-oss-120b"},
		{Profile: "coding", Provider: providerOpenRouter, Model: "inclusionai/ling-3.0-flash-fin:free"},
	}
	executor := &fakeAttemptExecutor{
		errs: []error{
			&attemptError{StatusCode: http.StatusPaymentRequired, Message: "upstream unavailable", ProviderUnavailable: true},
			nil,
		},
	}
	fallback := DefaultFallbackExecutor{AttemptExecutor: executor}

	err := fallback.Execute(context.Background(), httptest.NewRecorder(), FallbackExecution{
		RequestBody: map[string]interface{}{"messages": []interface{}{}},
		APIKey:      "test-key",
		Attempts:    attempts,
	})

	if err != nil {
		t.Fatalf("chain should have been served by the next provider, got %v", err)
	}
	if len(executor.attempts) != 2 {
		t.Fatalf("expected the chain to reach the second provider, attempts=%+v", executor.attempts)
	}
	if executor.attempts[1].Provider != providerGroq {
		t.Fatalf("expected Groq to be tried after Cerebras demanded payment, got %+v", executor.attempts[1])
	}
}
