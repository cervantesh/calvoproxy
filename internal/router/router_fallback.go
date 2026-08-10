package router

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
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
	reason           chainFailureReason
	err              error
	providerFailures []providerFailure
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

type providerFailure struct {
	Provider providerID
	Error    *attemptError
}

func (e DefaultFallbackExecutor) Execute(ctx context.Context, w http.ResponseWriter, execution FallbackExecution) error {
	if e.AttemptExecutor == nil {
		return &chainError{reason: chainExecutorError, err: errors.New("fallback attempt executor is not configured")}
	}

	var lastErr error
	unavailableProviders := map[providerID]struct{}{}
	unavailablePools := map[string]struct{}{}
	unavailableAttempts := map[int]struct{}{}
	providerFailures := make([]providerFailure, 0, 3)
	reportedProviderFailure := make(map[providerID]struct{}, 3)
	// stoppedEarly, not "did we break": a non-retryable error on the LAST model
	// breaks out of the loop with nothing left untried, which is diagnostically
	// "exhausted" — the chain got its full run. What makes "terminal" worth
	// alerting on is that models REMAINED, so the failure cost the request
	// options it never spent.
	stoppedEarly := false
	defer func() {
		for _, attempt := range execution.Attempts {
			if attempt.QuotaTicket.Valid() {
				attempt.QuotaTicket.ledger.Release(attempt.QuotaTicket, time.Now())
			}
		}
	}()
	for attemptIndex := range execution.Attempts {
		attempt := execution.Attempts[attemptIndex]
		if _, unavailable := unavailableAttempts[attemptIndex]; unavailable {
			continue
		}
		if _, unavailable := unavailableProviders[attempt.Provider]; unavailable {
			continue
		}
		if quotaPoolUnavailable(attempt, unavailablePools) {
			continue
		}
		slog.DebugContext(ctx, "[CalvoProxy] Executing attempt", slog.String("profile", attempt.Profile), slog.String("model", attempt.Model))
		requestBody := requestBodyForAttempt(execution.RequestBody, attempt)
		requestBody["model"] = attempt.Model
		upBytes, _ := json.Marshal(requestBody)
		attempt.AttemptIndex = attemptIndex + 1
		_, hasNextExecutable := nextExecutableAttempt(execution.Attempts, attemptIndex, unavailableProviders, unavailablePools, unavailableAttempts)
		attempt.LastInChain = !hasNextExecutable
		if hasNextExecutable && execution.ReserveQuota != nil {
			attempt.ReserveFallback = func() bool {
				_, ok := reserveNextExecutableAttempt(&execution, attemptIndex, unavailableProviders, unavailablePools, unavailableAttempts)
				return ok
			}
		}

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
			if execution.OnSuccess != nil {
				execution.OnSuccess(attempt)
			}
			return nil
		} else {
			if execution.OnFailure != nil {
				execution.OnFailure(attempt)
			}
			lastErr = err
			slog.WarnContext(ctx, "[CalvoProxy] ⚠️ Fallback", slog.String("model", attempt.Model), slog.Any("error", err))
			var attErr *attemptError
			if errors.As(err, &attErr) {
				if attErr.ProviderUnavailable {
					unavailableProviders[attempt.Provider] = struct{}{}
					if _, reported := reportedProviderFailure[attempt.Provider]; !reported {
						providerFailures = append(providerFailures, providerFailure{Provider: attempt.Provider, Error: attErr})
						reportedProviderFailure[attempt.Provider] = struct{}{}
					}
					continue
				}
				if attErr.QuotaLimited {
					if _, reported := reportedProviderFailure[attempt.Provider]; !reported {
						providerFailures = append(providerFailures, providerFailure{Provider: attempt.Provider, Error: attErr})
						reportedProviderFailure[attempt.Provider] = struct{}{}
					}
				}
				if attErr.QuotaPool != "" {
					unavailablePools[quotaPoolKey(attempt.Provider, attErr.QuotaPool)] = struct{}{}
					continue
				}
				// Model-specific unavailability (a retired OpenRouter :free slug
				// → 404) or a soft skip (half-open probe already in flight): jump
				// straight to the next model, no backoff — not the request's fault.
				if isModelUnavailable(attErr) || attErr.SkipModel {
					continue
				}
				// Otherwise honour the retry policy: a terminal error that
				// every model would hit (auth, malformed request) stops here.
				if !shouldRetryAttempt(execution.RetryPolicy, attErr) {
					_, stoppedEarly = nextExecutableAttempt(execution.Attempts, attemptIndex, unavailableProviders, unavailablePools, unavailableAttempts)
					break
				}
			}
			// A different provider is an immediately usable fallback, not a
			// retry of the failing upstream. Do not make it pay that upstream's
			// backoff. Keep the existing backoff between models on the same
			// provider, where it still protects that shared upstream.
			if next, ok := nextExecutableAttempt(execution.Attempts, attemptIndex, unavailableProviders, unavailablePools, unavailableAttempts); ok && next.Provider == attempt.Provider {
				waitBeforeRetry(ctx, execution.RetryPolicy, attemptIndex)
			}
		}
	}

	reason := chainExhausted
	if stoppedEarly {
		reason = chainTerminal
	}
	if lastErr != nil {
		return &chainError{reason: reason, err: lastErr, providerFailures: providerFailures}
	}
	// Reached only with an empty Attempts slice. dispatchChain catches that case
	// before calling the executor (it is the "all models cooling down" 503, which
	// has its own counter), so in the served path this is unreachable; it stays
	// as the executor's own contract for a direct caller.
	return &chainError{reason: chainExhausted, err: errAllFallbackModelsFailed}
}

// nextExecutableAttempt returns the next candidate that has not been removed
// by provider-wide evidence learned earlier in this chain. The raw slice tail
// is not necessarily executable: it may consist entirely of sibling models
// belonging to a provider whose account-wide quota has already been exhausted.
func nextExecutableAttempt(attempts []modelAttempt, currentIndex int, unavailableProviders map[providerID]struct{}, unavailablePools map[string]struct{}, unavailableAttempts map[int]struct{}) (modelAttempt, bool) {
	for i := currentIndex + 1; i < len(attempts); i++ {
		if _, unavailable := unavailableAttempts[i]; unavailable {
			continue
		}
		candidate := attempts[i]
		if _, unavailable := unavailableProviders[candidate.Provider]; unavailable {
			continue
		}
		if quotaPoolUnavailable(candidate, unavailablePools) {
			continue
		}
		return candidate, true
	}
	return modelAttempt{}, false
}

func reserveNextExecutableAttempt(execution *FallbackExecution, currentIndex int, unavailableProviders map[providerID]struct{}, unavailablePools map[string]struct{}, unavailableAttempts map[int]struct{}) (modelAttempt, bool) {
	for i := currentIndex + 1; i < len(execution.Attempts); i++ {
		if _, unavailable := unavailableAttempts[i]; unavailable {
			continue
		}
		candidate := execution.Attempts[i]
		if _, unavailable := unavailableProviders[candidate.Provider]; unavailable {
			continue
		}
		if quotaPoolUnavailable(candidate, unavailablePools) {
			continue
		}
		if execution.ReserveQuota != nil && !candidate.QuotaTicket.Valid() {
			ticket, ok := execution.ReserveQuota(candidate)
			if !ok {
				unavailableAttempts[i] = struct{}{}
				continue
			}
			candidate.QuotaTicket = ticket
			execution.Attempts[i] = candidate
		}
		return candidate, true
	}
	return modelAttempt{}, false
}

func quotaPoolKey(provider providerID, pool string) string {
	return string(provider) + "\x00" + pool
}

func quotaPoolUnavailable(attempt modelAttempt, unavailablePools map[string]struct{}) bool {
	if strings.HasSuffix(strings.ToLower(attempt.Model), ":free") {
		_, unavailable := unavailablePools[quotaPoolKey(attempt.Provider, quotaFreePool)]
		return unavailable
	}
	return false
}

func fallbackErrorResponse(err error) (int, string) {
	statusCode := http.StatusBadGateway
	message := errAllFallbackModelsFailed.Error()
	if err != nil {
		var chainErr *chainError
		if errors.As(err, &chainErr) && len(chainErr.providerFailures) > 1 {
			return http.StatusServiceUnavailable, multiProviderUnavailableMessage(chainErr.providerFailures)
		}
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

func fallbackRetryAfter(err error) time.Duration {
	var chainErr *chainError
	if !errors.As(err, &chainErr) {
		return 0
	}
	var soonest time.Duration
	for _, failure := range chainErr.providerFailures {
		if failure.Error == nil || failure.Error.RetryAfter <= 0 {
			continue
		}
		if soonest == 0 || failure.Error.RetryAfter < soonest {
			soonest = failure.Error.RetryAfter
		}
	}
	return soonest
}

func multiProviderUnavailableMessage(failures []providerFailure) string {
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, providerDisplayName(failure.Provider)+": "+safeProviderFailureSummary(failure.Error))
	}
	return "All configured model providers are temporarily rate-limited or unavailable. " + strings.Join(parts, "; ") + ". Automatic routing will retry them after their cooldowns."
}

func providerDisplayName(provider providerID) string {
	switch provider {
	case providerOpenRouter:
		return "OpenRouter"
	case providerCerebras:
		return "Cerebras"
	case providerGroq:
		return "Groq"
	default:
		return string(provider)
	}
}

func safeProviderFailureSummary(err *attemptError) string {
	if err == nil {
		return "unavailable"
	}
	if strings.HasPrefix(err.Message, openRouterDailyFreeQuotaPrefix) {
		if resetAt := strings.Index(err.Message, "Resets: "); resetAt >= 0 {
			reset := err.Message[resetAt+len("Resets: "):]
			if end := strings.Index(reset, "."); end >= 0 {
				reset = reset[:end]
			}
			return "daily free-model quota exhausted; resets " + reset
		}
		return "daily free-model quota exhausted"
	}
	if err.Timeout {
		return "timed out"
	}
	switch err.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication or account authorization failed"
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return "rate limited or temporarily unavailable"
	default:
		if err.StatusCode >= 500 {
			return "temporarily unavailable"
		}
		return "request rejected"
	}
}

type routerAttemptExecutor struct {
	service *RouterService
}

func (e routerAttemptExecutor) ExecuteAttempt(ctx context.Context, w http.ResponseWriter, body []byte, apiKey string, attempt modelAttempt) error {
	return e.service.executeAttempt(ctx, w, body, apiKey, attempt)
}

func (s *RouterService) executeFallbacks(ctx context.Context, w http.ResponseWriter, execution FallbackExecution) error {
	if execution.OnSuccess == nil {
		execution.OnSuccess = func(attempt modelAttempt) { s.recordAffinitySuccess(ctx, attempt) }
	}
	if execution.OnFailure == nil {
		execution.OnFailure = func(attempt modelAttempt) { s.recordAffinityFailure(ctx, attempt) }
	}
	if execution.ReserveQuota == nil {
		baseEstimate := estimateRequestQuota(execution.RequestBody)
		execution.ReserveQuota = func(attempt modelAttempt) (QuotaTicket, bool) {
			credential, configured := s.providerCredential(ctx, attempt, execution.APIKey)
			if !configured {
				return QuotaTicket{}, false
			}
			estimate := attempt.QuotaEstimate
			if estimate.Requests <= 0 || estimate.Tokens <= 0 {
				estimate = providerQuotaEstimate(attempt, baseEstimate)
			}
			return s.reserveFallbackQuota(attempt, credential, estimate, time.Now())
		}
	}
	executor := s.Fallbacks
	if executor == nil {
		executor = DefaultFallbackExecutor{AttemptExecutor: routerAttemptExecutor{service: s}}
	}
	return executor.Execute(ctx, w, execution)
}
