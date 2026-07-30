package router

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

var errAllFallbackModelsFailed = errors.New("all fallback models failed")

// isModelUnavailable reports whether the upstream rejected this specific model
// (rather than the request as a whole). The canonical case is OpenRouter
// returning 404 "This model is unavailable for free" once it retires a :free
// slug, but it also covers model-not-found / no-endpoints errors. Such
// failures are model-specific, so the fallback chain must advance to the NEXT
// model instead of aborting — a sibling model in the chain may well succeed.
func isModelUnavailable(err *attemptError) bool {
	if err == nil {
		return false
	}
	if err.StatusCode == http.StatusNotFound {
		return true
	}
	if err.StatusCode == http.StatusBadRequest {
		m := strings.ToLower(err.Message)
		if strings.Contains(m, "no endpoints found") ||
			strings.Contains(m, "not a valid model") ||
			strings.Contains(m, "no allowed providers") ||
			strings.Contains(m, "unavailable") {
			return true
		}
	}
	return false
}

type DefaultFallbackExecutor struct {
	AttemptExecutor AttemptExecutor
}

func (e DefaultFallbackExecutor) Execute(ctx context.Context, w http.ResponseWriter, execution FallbackExecution) error {
	if e.AttemptExecutor == nil {
		return errors.New("fallback attempt executor is not configured")
	}

	var lastErr error
	for attemptIndex, attempt := range execution.Attempts {
		slog.DebugContext(ctx, "[CervoProxy] Executing attempt", slog.String("profile", attempt.Profile), slog.String("model", attempt.Model))
		execution.RequestBody["model"] = attempt.Model
		upBytes, _ := json.Marshal(execution.RequestBody)

		if err := e.AttemptExecutor.ExecuteAttempt(ctx, w, upBytes, execution.APIKey, attempt); err == nil {
			return nil
		} else {
			lastErr = err
			slog.WarnContext(ctx, "[CervoProxy] ⚠️ Fallback", slog.String("model", attempt.Model), slog.Any("error", err))
			var attErr *attemptError
			if errors.As(err, &attErr) {
				// Model-specific unavailability (e.g. a retired OpenRouter
				// :free slug → 404): skip straight to the next model in the
				// chain, no backoff — the failure is not the request's fault.
				if isModelUnavailable(attErr) {
					continue
				}
				// Otherwise honour the retry policy: a terminal error that
				// every model would hit (auth, malformed request) stops here.
				if !shouldRetryAttempt(execution.RetryPolicy, attErr) {
					break
				}
			}
			if attemptIndex < len(execution.Attempts)-1 {
				waitBeforeRetry(ctx, execution.RetryPolicy, attemptIndex)
			}
		}
	}

	if lastErr != nil {
		return lastErr
	}
	return errAllFallbackModelsFailed
}

func fallbackErrorResponse(err error) (int, string) {
	statusCode := http.StatusBadGateway
	message := errAllFallbackModelsFailed.Error()
	if err != nil {
		message = err.Error()
		var attErr *attemptError
		if errors.As(err, &attErr) {
			if attErr.StatusCode != 0 {
				statusCode = attErr.StatusCode
			}
			message = attErr.Message
		}
	}
	return statusCode, message
}

type routerAttemptExecutor struct {
	service *RouterService
}

func (e routerAttemptExecutor) ExecuteAttempt(ctx context.Context, w http.ResponseWriter, body []byte, apiKey string, attempt modelAttempt) error {
	return e.service.executeAttempt(ctx, w, body, apiKey, attempt)
}

func (s *RouterService) executeFallbacks(ctx context.Context, w http.ResponseWriter, execution FallbackExecution) error {
	executor := s.Fallbacks
	if executor == nil {
		executor = DefaultFallbackExecutor{AttemptExecutor: routerAttemptExecutor{service: s}}
	}
	return executor.Execute(ctx, w, execution)
}
