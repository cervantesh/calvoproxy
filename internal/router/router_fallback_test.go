package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeAttemptExecutor struct {
	errs     []error
	attempts []modelAttempt
	bodies   [][]byte
}

func (e *fakeAttemptExecutor) ExecuteAttempt(_ context.Context, _ http.ResponseWriter, body []byte, _ string, attempt modelAttempt) error {
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
