package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeAttemptExecutor struct {
	errs                []error
	attempts            []modelAttempt
	bodies              [][]byte
	invokeFallbackGuard bool
	guardResults        []bool
}

func (e *fakeAttemptExecutor) ExecuteAttempt(_ context.Context, _ http.ResponseWriter, body []byte, _ string, attempt modelAttempt) error {
	if e.invokeFallbackGuard && attempt.ReserveFallback != nil {
		available := attempt.ReserveFallback()
		e.guardResults = append(e.guardResults, available)
		if !available {
			attempt.LastInChain = true
		}
	}
	e.attempts = append(e.attempts, attempt)
	e.bodies = append(e.bodies, append([]byte(nil), body...))
	if len(e.errs) == 0 {
		return nil
	}
	err := e.errs[0]
	e.errs = e.errs[1:]
	return err
}

func TestDefaultFallbackExecutorStopsAfterSuccessfulAttempt(t *testing.T) {
	attempts := []modelAttempt{
		{Profile: "simple", Model: "bad-model"},
		{Profile: "simple", Model: "good-model"},
		{Profile: "simple", Model: "unused-model"},
	}
	executor := &fakeAttemptExecutor{
		errs: []error{
			&attemptError{StatusCode: http.StatusBadGateway, Message: "bad gateway", Retryable: true},
			nil,
		},
	}
	fallback := DefaultFallbackExecutor{AttemptExecutor: executor}

	err := fallback.Execute(context.Background(), httptest.NewRecorder(), FallbackExecution{
		RequestBody: map[string]interface{}{"messages": []interface{}{}},
		APIKey:      "test-key",
		Attempts:    attempts,
		RetryPolicy: RetryPolicy{
			RetryHTTPStatuses: []int{http.StatusBadGateway},
		},
	})

	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}
	if len(executor.attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %+v", executor.attempts)
	}
	if executor.attempts[1].Model != "good-model" {
		t.Fatalf("expected second attempt to use good-model, got %+v", executor.attempts[1])
	}
	if string(executor.bodies[1]) != `{"messages":[],"model":"good-model"}` {
		t.Fatalf("expected request body model mutation, got %s", executor.bodies[1])
	}
}

func TestDefaultFallbackExecutorStopsOnNonRetryableAttemptError(t *testing.T) {
	executor := &fakeAttemptExecutor{
		errs: []error{
			&attemptError{StatusCode: http.StatusUnauthorized, Message: "unauthorized", Retryable: false},
		},
	}
	fallback := DefaultFallbackExecutor{AttemptExecutor: executor}

	err := fallback.Execute(context.Background(), httptest.NewRecorder(), FallbackExecution{
		RequestBody: map[string]interface{}{"messages": []interface{}{}},
		APIKey:      "test-key",
		Attempts: []modelAttempt{
			{Profile: "simple", Model: "first"},
			{Profile: "simple", Model: "second"},
		},
		RetryPolicy: RetryPolicy{RetryHTTPStatuses: []int{http.StatusBadGateway}},
	})

	if err == nil {
		t.Fatal("expected non-retryable error")
	}
	if len(executor.attempts) != 1 {
		t.Fatalf("expected non-retryable error to stop after 1 attempt, got %+v", executor.attempts)
	}
}

func TestDefaultFallbackExecutorAdvancesOnModelUnavailable(t *testing.T) {
	// A 404 "model unavailable" (retired OpenRouter :free slug) is model-
	// specific and is deliberately NOT listed in RetryHTTPStatuses. The chain
	// must still advance to the next model instead of aborting.
	executor := &fakeAttemptExecutor{
		errs: []error{
			&attemptError{StatusCode: http.StatusNotFound, Message: "This model is unavailable for free", Retryable: false},
			nil,
		},
	}
	fallback := DefaultFallbackExecutor{AttemptExecutor: executor}

	err := fallback.Execute(context.Background(), httptest.NewRecorder(), FallbackExecution{
		RequestBody: map[string]interface{}{"messages": []interface{}{}},
		APIKey:      "test-key",
		Attempts: []modelAttempt{
			{Profile: "coding", Model: "retired:free"},
			{Profile: "coding", Model: "working:free"},
		},
		RetryPolicy: RetryPolicy{RetryHTTPStatuses: []int{http.StatusBadGateway}},
	})

	if err != nil {
		t.Fatalf("expected fallback to advance past unavailable model, got %v", err)
	}
	if len(executor.attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %+v", executor.attempts)
	}
	if executor.attempts[1].Model != "working:free" {
		t.Fatalf("expected advance to working model, got %+v", executor.attempts[1])
	}
}

func TestDefaultFallbackExecutorMarksLastActuallyExecutableAttemptAfterProviderExclusion(t *testing.T) {
	executor := &fakeAttemptExecutor{
		errs: []error{
			&attemptError{
				StatusCode:          http.StatusTooManyRequests,
				Message:             openRouterDailyFreeQuotaPrefix,
				Retryable:           true,
				ProviderUnavailable: true,
			},
			nil,
		},
	}
	fallback := DefaultFallbackExecutor{AttemptExecutor: executor}

	err := fallback.Execute(context.Background(), httptest.NewRecorder(), FallbackExecution{
		RequestBody: map[string]interface{}{"messages": []interface{}{}},
		APIKey:      "test-key",
		Attempts: []modelAttempt{
			{Profile: "coding", Provider: providerOpenRouter, Model: "or-primary:free"},
			{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b"},
			// This is the raw slice tail, but the first result excluded its
			// provider globally, so it must not prevent Cerebras from being
			// treated as the final executable attempt.
			{Profile: "coding", Provider: providerOpenRouter, Model: "or-sibling:free"},
		},
		RetryPolicy: RetryPolicy{RetryHTTPStatuses: []int{http.StatusTooManyRequests}},
	})

	if err != nil {
		t.Fatalf("expected Cerebras fallback success, got %v", err)
	}
	if len(executor.attempts) != 2 {
		t.Fatalf("expected OpenRouter and Cerebras only, got %+v", executor.attempts)
	}
	if executor.attempts[0].LastInChain {
		t.Fatal("OpenRouter primary must not be last while Cerebras remains executable")
	}
	if got := executor.attempts[1]; got.Provider != providerCerebras || !got.LastInChain {
		t.Fatalf("expected Cerebras to be the last executable attempt, got %+v", got)
	}
}

func TestDefaultFallbackExecutorMarksCurrentLastWhenFutureQuotaReservationLosesRace(t *testing.T) {
	executor := &fakeAttemptExecutor{errs: []error{nil}, invokeFallbackGuard: true}
	fallback := DefaultFallbackExecutor{AttemptExecutor: executor}
	reservationCalls := 0

	err := fallback.Execute(context.Background(), httptest.NewRecorder(), FallbackExecution{
		RequestBody: map[string]interface{}{"messages": []interface{}{}},
		APIKey:      "test-key",
		Attempts: []modelAttempt{
			{Profile: "coding", Provider: providerOpenRouter, Model: "current:free"},
			{Profile: "coding", Provider: providerCerebras, Model: "raced-out"},
		},
		ReserveQuota: func(modelAttempt) (QuotaTicket, bool) {
			reservationCalls++
			return QuotaTicket{}, false
		},
	})

	if err != nil {
		t.Fatalf("expected current attempt success, got %v", err)
	}
	if reservationCalls != 1 {
		t.Fatalf("expected one atomic look-ahead reservation, got %d", reservationCalls)
	}
	if len(executor.guardResults) != 1 || executor.guardResults[0] {
		t.Fatalf("fail-fast guard must reject abandonment without reserved fallback: %v", executor.guardResults)
	}
	if len(executor.attempts) != 1 || !executor.attempts[0].LastInChain {
		t.Fatalf("current attempt must be protected as last after the fallback loses quota: %+v", executor.attempts)
	}
}

func TestDefaultFallbackExecutorDoesNotReserveFallbackDuringHealthyPrimary(t *testing.T) {
	executor := &fakeAttemptExecutor{errs: []error{nil}}
	fallback := DefaultFallbackExecutor{AttemptExecutor: executor}
	reservationCalls := 0

	err := fallback.Execute(context.Background(), httptest.NewRecorder(), FallbackExecution{
		RequestBody: map[string]interface{}{"messages": []interface{}{}},
		APIKey:      "test-key",
		Attempts: []modelAttempt{
			{Profile: "coding", Provider: providerOpenRouter, Model: "healthy:free"},
			{Profile: "coding", Provider: providerCerebras, Model: "unused"},
		},
		ReserveQuota: func(modelAttempt) (QuotaTicket, bool) {
			reservationCalls++
			return QuotaTicket{}, false
		},
	})

	if err != nil {
		t.Fatalf("expected primary success, got %v", err)
	}
	if reservationCalls != 0 {
		t.Fatalf("healthy primary must not reserve unused fallback quota, got %d calls", reservationCalls)
	}
}

func TestDefaultFallbackExecutorDoesNotBackoffBeforeAvailableDifferentProvider(t *testing.T) {
	executor := &fakeAttemptExecutor{
		errs: []error{
			&attemptError{StatusCode: http.StatusBadGateway, Message: "first failed", Retryable: true},
			&attemptError{StatusCode: http.StatusBadGateway, Message: "second failed", Retryable: true},
			nil,
		},
	}
	fallback := DefaultFallbackExecutor{AttemptExecutor: executor}
	started := time.Now()

	err := fallback.Execute(context.Background(), httptest.NewRecorder(), FallbackExecution{
		RequestBody: map[string]interface{}{"messages": []interface{}{}},
		APIKey:      "test-key",
		Attempts: []modelAttempt{
			{Profile: "coding", Provider: providerOpenRouter, Model: "or-one:free"},
			{Profile: "coding", Provider: providerOpenRouter, Model: "or-two:free"},
			{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b"},
		},
		RetryPolicy: RetryPolicy{
			RetryHTTPStatuses: []int{http.StatusBadGateway},
			BackoffMin:        500 * time.Millisecond,
			BackoffMax:        500 * time.Millisecond,
		},
	})

	if err != nil {
		t.Fatalf("expected different-provider fallback success, got %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond {
		t.Fatalf("different provider was delayed by retry backoff: %v", elapsed)
	}
}

func TestIsModelUnavailableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  *attemptError
		want bool
	}{
		{"404", &attemptError{StatusCode: http.StatusNotFound, Message: "unavailable for free"}, true},
		{"400 no endpoints", &attemptError{StatusCode: http.StatusBadRequest, Message: "No endpoints found for model x"}, true},
		{"401 auth", &attemptError{StatusCode: http.StatusUnauthorized, Message: "unauthorized"}, false},
		{"429 rate", &attemptError{StatusCode: http.StatusTooManyRequests, Message: "rate limited"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isModelUnavailable(tc.err); got != tc.want {
			t.Fatalf("%s: isModelUnavailable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFallbackErrorResponseUsesAttemptErrorStatus(t *testing.T) {
	status, message := fallbackErrorResponse(&attemptError{StatusCode: http.StatusTooManyRequests, Message: "rate limited"})

	if status != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", status)
	}
	if message != "rate limited" {
		t.Fatalf("expected attempt error message, got %q", message)
	}
}

func TestFallbackErrorResponseDefaultsToBadGateway(t *testing.T) {
	status, message := fallbackErrorResponse(errors.New("boom"))

	if status != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", status)
	}
	if message != "boom" {
		t.Fatalf("expected original error message, got %q", message)
	}
}

func TestFallbackErrorResponseSummarizesMultipleProvidersWithoutRawDetails(t *testing.T) {
	err := &chainError{
		reason: chainExhausted,
		err:    errors.New("raw final provider response containing secret-token"),
		providerFailures: []providerFailure{
			{Provider: providerOpenRouter, Error: &attemptError{StatusCode: http.StatusTooManyRequests, Message: openRouterDailyFreeQuotaPrefix + " (50/50). Resets: 2026-08-09 00:00 UTC.", ProviderUnavailable: true}},
			{Provider: providerCerebras, Error: &attemptError{StatusCode: http.StatusTooManyRequests, Message: "raw-cerebras-secret", ProviderUnavailable: true}},
			{Provider: providerGroq, Error: &attemptError{StatusCode: http.StatusUnauthorized, Message: "raw-groq-secret", ProviderUnavailable: true}},
		},
	}
	status, message := fallbackErrorResponse(err)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", status)
	}
	for _, expected := range []string{"All configured model providers are temporarily rate-limited or unavailable", "OpenRouter: daily free-model quota exhausted", "Cerebras: rate limited", "Groq: authentication"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("missing %q in %q", expected, message)
		}
	}
	for _, forbidden := range []string{"secret-token", "raw-cerebras-secret", "raw-groq-secret"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("aggregate message leaked raw provider details %q: %q", forbidden, message)
		}
	}
}
