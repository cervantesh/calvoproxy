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

// chainError carries the fallback loop's own verdict on WHY it stopped, which
// the returned error otherwise loses: a break (a terminal error) and a normal
// end of loop (everything tried) both `return lastErr`, so the caller cannot
// tell them apart, and the error's shape does not say either — a cancelled
// attempt and a genuinely terminal one are both Retryable:false.
//
// It is only the loop's verdict, not the final answer. Context state outranks
// it; see classifyChainFailure.
type chainError struct {
	reason chainFailureReason
	err    error
}

func (e *chainError) Error() string { return e.err.Error() }
func (e *chainError) Unwrap() error { return e.err }

// classifyChainFailure names why a chain failed, for
// calvoproxy_chain_failed_total.
//
// ctx.Err() is checked FIRST and wins over the loop's verdict, because the loop
// physically cannot see a client disconnect. executeAttempt converts a cancelled
// parent context into attemptError{Retryable:false, SkipModel:true}, and the
// loop treats SkipModel as `continue` — so a client hanging up does not stop the
// chain. It burns every remaining model, one cancelled attempt at a time, and
// leaves through the normal end of the loop, where the verdict reads
// "exhausted". Inferring from the error instead is no better: Retryable:false
// reads as "terminal", and DeadlineExceeded has the same shape as Canceled but
// is deliberately excluded from that branch in executeAttempt.
//
// Caveat on total_timeout: this reads the context dispatchChain built, whose
// deadline is the total chain budget — but that budget is only applied when
// !stream (see dispatchChain), so on a streaming-heavy instance this bucket
// will sit at or near zero and is NOT evidence that chains never time out.
// Per-attempt deadlines cannot leak in here: they live on a child context.
func classifyChainFailure(ctx context.Context, err error) chainFailureReason {
	if errors.Is(ctx.Err(), context.Canceled) {
		return chainCancelled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return chainTotalTimeout
	}
	var chainErr *chainError
	if errors.As(err, &chainErr) {
		return chainErr.reason
	}
	// A custom FallbackExecutor returning a bare error: everything was tried as
	// far as we can tell.
	return chainExhausted
}

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
		return &chainError{reason: chainExecutorError, err: errors.New("fallback attempt executor is not configured")}
	}

	var lastErr error
	// stoppedEarly, not "did we break": a non-retryable error on the LAST model
	// breaks out of the loop with nothing left untried, which is diagnostically
	// "exhausted" — the chain got its full run. What makes "terminal" worth
	// alerting on is that models REMAINED, so the failure cost the request
	// options it never spent.
	stoppedEarly := false
	for attemptIndex, attempt := range execution.Attempts {
		slog.DebugContext(ctx, "[CalvoProxy] Executing attempt", slog.String("profile", attempt.Profile), slog.String("model", attempt.Model))
		execution.RequestBody["model"] = attempt.Model
		upBytes, _ := json.Marshal(execution.RequestBody)
		attempt.AttemptIndex = attemptIndex + 1
		attempt.LastInChain = attemptIndex == len(execution.Attempts)-1

		// Each non-streaming attempt gets its own deadline so a slow model is cut
		// at PerAttemptTimeout, leaving the rest of the overall budget for the
		// fallbacks. Streaming attempts are NOT capped here — they run under the
		// parent context, bounded by header + idle timeouts (see streamCopy).
		attemptCtx := ctx
		var cancel context.CancelFunc
		if !execution.Stream && execution.PerAttemptTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, execution.PerAttemptTimeout)
		}
		err := e.AttemptExecutor.ExecuteAttempt(attemptCtx, w, upBytes, execution.APIKey, attempt)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return nil
		} else {
			lastErr = err
			slog.WarnContext(ctx, "[CalvoProxy] ⚠️ Fallback", slog.String("model", attempt.Model), slog.Any("error", err))
			var attErr *attemptError
			if errors.As(err, &attErr) {
				// Model-specific unavailability (a retired OpenRouter :free slug
				// → 404) or a soft skip (half-open probe already in flight): jump
				// straight to the next model, no backoff — not the request's fault.
				if isModelUnavailable(attErr) || attErr.SkipModel {
					continue
				}
				// Otherwise honour the retry policy: a terminal error that
				// every model would hit (auth, malformed request) stops here.
				if !shouldRetryAttempt(execution.RetryPolicy, attErr) {
					stoppedEarly = attemptIndex < len(execution.Attempts)-1
					break
				}
			}
			if attemptIndex < len(execution.Attempts)-1 {
				waitBeforeRetry(ctx, execution.RetryPolicy, attemptIndex)
			}
		}
	}

	reason := chainExhausted
	if stoppedEarly {
		reason = chainTerminal
	}
	if lastErr != nil {
		return &chainError{reason: reason, err: lastErr}
	}
	// Reached only with an empty Attempts slice. dispatchChain catches that case
	// before calling the executor (it is the "all models cooling down" 503, which
	// has its own counter), so in the served path this is unreachable; it stays
	// as the executor's own contract for a direct caller.
	return &chainError{reason: chainExhausted, err: errAllFallbackModelsFailed}
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
