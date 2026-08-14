package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

func (s *RouterService) executeAttempt(ctx context.Context, w http.ResponseWriter, body []byte, apiKey string, attempt modelAttempt) error {
	tracer := otel.Tracer("calvoproxy/router")
	ctx, span := tracer.Start(ctx, "Execute_LLM_Attempt: "+attempt.Model)
	span.SetAttributes(
		attribute.String("llm.model", attempt.Model),
		attribute.String("llm.profile", attempt.Profile),
	)
	defer span.End()

	providerKey, configured := s.providerCredential(ctx, attempt, apiKey)
	if !configured {
		return &attemptError{StatusCode: http.StatusServiceUnavailable, Retryable: true, SkipModel: true, Message: "provider " + string(attempt.Provider) + " is not configured"}
	}
	estimate := attempt.QuotaEstimate
	if estimate.Requests <= 0 || estimate.Tokens <= 0 {
		var requestBody map[string]interface{}
		_ = json.Unmarshal(body, &requestBody)
		estimate = providerQuotaEstimate(attempt, estimateRequestQuota(requestBody))
		attempt.QuotaEstimate = estimate
	}
	ticket := attempt.QuotaTicket
	sent, settled := false, false
	defer func() {
		if !ticket.Valid() || settled {
			return
		}
		if !sent {
			s.quotaLedger().Release(ticket, time.Now())
			return
		}
		// The request left the process but no response proved its exact usage.
		// Commit the estimate conservatively; the unique ticket prevents any
		// duplicate cleanup from releasing another request's reservation.
		s.observeAndSettleQuota(ticket, attempt, providerKey, 0, nil, nil, estimate.Tokens, time.Now())
	}()
	if !s.tryStartProvider(attempt) {
		return &attemptError{StatusCode: http.StatusServiceUnavailable, Retryable: true, SkipModel: true, Message: "provider " + string(attempt.Provider) + " is cooling down"}
	}

	// Single-flight the half-open recovery probe: if this model's circuit just
	// entered its half-open window and another request already claimed the probe,
	// skip to the next model instead of stampeding the recovering upstream. This
	// is a soft skip (retryable, not breaker-eligible, no score penalty).
	if !s.tryStartAttempt(attempt) {
		s.resolveProviderProbe(attempt.Provider)
		recordTraceFailure(ctx, attempt, 0, "probe", "recovery probe already in flight")
		return &attemptError{StatusCode: http.StatusServiceUnavailable, Retryable: true, SkipModel: true, Message: "recovery probe already in flight for " + attempt.Model}
	}

	opPath := attempt.Path
	if opPath == "" {
		opPath = chatCompletionsPath
	}
	target := s.resolveAttemptTarget(attempt, opPath)
	if target.Agentic {
		slog.InfoContext(ctx, "[CalvoProxy] 🛠️ Routing agentic request to GeminiCLIAPI", slog.String("profile", attempt.Profile))
	}

	proxyReq, err := newUpstreamRequest(ctx, http.MethodPost, target.URL, body, providerKey)
	if err != nil {
		s.resolveProbe(attempt)
		s.resolveProviderProbe(attempt.Provider)
		return classifyTransportError(err)
	}
	if !ticket.Valid() {
		var ok bool
		ticket, ok = s.reserveFallbackQuota(attempt, providerKey, estimate, time.Now())
		if !ok {
			s.resolveProbe(attempt)
			s.resolveProviderProbe(attempt.Provider)
			return &attemptError{StatusCode: http.StatusTooManyRequests, Retryable: true, SkipModel: true, QuotaLimited: true, Message: providerDisplayName(attempt.Provider) + " is temporarily rate-limited for this model; trying another provider"}
		}
	}

	// Clock for time-to-first-token, started BEFORE the upstream call so it
	// includes the wait for response headers. Only the streaming branch below
	// consumes it, so a non-streaming attempt — whose headers arrive when
	// generation is essentially finished — can never contribute a sample.
	attemptStart := time.Now()
	s.recordProviderAttempt(attempt.Provider, attempt.BalanceReserved)
	sent = true
	resp, err := s.Client.Do(proxyReq)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Transport Error")
		// A cancelled request context is not the model's fault. The client hung
		// up, or we abandoned the attempt ourselves — either way the upstream
		// was never given a chance to fail. Scoring it, opening its circuit or
		// counting it against the shared host punishes a model for someone
		// else's disconnect.
		//
		// Same bug class fixed for streams in v0.4.0 (router_stream.go checks
		// ctx.Err() before blaming the model); the non-streaming path never got
		// the same treatment. It matters more than it looks: the vendored
		// ClassifyTransportError marks everything BreakerEligible by default
		// with no context.Canceled branch, and the host breaker counts ANY
		// transport error — so a few cancelled requests in a row open
		// openrouter.ai for EVERY model.
		//
		// DeadlineExceeded is deliberately excluded: that is our own per-attempt
		// timeout firing, which is real evidence the model is too slow.
		if errors.Is(ctx.Err(), context.Canceled) {
			s.resolveProbe(attempt) // release the half-open claim without a verdict
			s.resolveProviderProbe(attempt.Provider)
			return &attemptError{
				StatusCode: http.StatusServiceUnavailable,
				Retryable:  false,
				SkipModel:  true,
				Message:    "request cancelled before the upstream responded",
			}
		}
		attErr := classifyTransportError(err)
		recordTraceFailure(ctx, attempt, 0, "transport", attErr.Message)
		s.penalizeScore(attempt, attErr.StatusCode)
		if attErr.BreakerEligible {
			s.recordFailure(attempt, attErr.StatusCode, attErr.Message)
		}
		s.resolveProviderProbe(attempt.Provider)
		return attErr
	}
	defer func() { _ = resp.Body.Close() }()
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		// Error bodies are only used for classification/logging — cap the read so
		// a hostile/broken upstream can't flood memory with a giant error body.
		respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes()))
		if resp.StatusCode >= 500 {
			span.RecordError(errors.New(string(respBytes)))
			span.SetStatus(codes.Error, "Upstream HTTP Error")
		}
		attErr := classifyHTTPError(resp.StatusCode, string(respBytes))
		providerRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		quotaObservations := s.observeAndSettleQuota(ticket, attempt, providerKey, resp.StatusCode, resp.Header, respBytes, estimate.Tokens, time.Now())
		settled = true
		providerQuotaExhausted := false
		quotaLimited := resp.StatusCode == http.StatusTooManyRequests
		for _, observation := range quotaObservations {
			if observation.RetryAfter > providerRetryAfter {
				providerRetryAfter = observation.RetryAfter
			}
			if observation.Exhausted && observation.Scope == providerQuotaScopeFreePool {
				providerQuotaExhausted = true
			}
		}
		providerAuthFailure := (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && !isProviderRelayedError(string(respBytes))
		if providerAuthFailure && attempt.Provider != providerOpenRouter {
			attErr.ProviderUnavailable = true
		}
		if quotaLimited {
			attErr.QuotaLimited = true
			attErr.SkipModel = true
			providerRetryAfter = s.setQuotaCooldown(attempt, providerRetryAfter, time.Now())
			attErr.RetryAfter = providerRetryAfter
			if providerQuotaExhausted {
				attErr.QuotaPool = quotaFreePool
			}
			s.resolveProbe(attempt)
			s.resolveProviderProbe(attempt.Provider)
		}
		// A 400 from ONE upstream is not a verdict on the request. The same body
		// goes to providers with different limits, so "invalid" here can be
		// perfectly valid there — and 400 is otherwise terminal, which ends the
		// whole chain on the first picky provider.
		//
		// Observed: a client exposing more than 64 tools received
		//   400 invalid_request_error: "at most 64 tools are allowed"
		// from one provider. Every other model in the chain would have served the
		// request, but the chain stopped and the user got an error in 0.8s.
		//
		// Advancing costs at most K fast attempts when the request really is
		// malformed — every model rejects it immediately — and the client still
		// gets a 400 at the end. Worth it against losing a request a later model
		// would have answered.
		// A 401 arrived the same way on 2026-08-03: OpenRouter relaying
		//   401 authentication_error: "invalid API key"
		// from the same provider, while the account's own key was valid and
		// answering /api/v1/key with 200. 401 is terminal, so the chain died on
		// a provider-side credential problem that the next model — routed to a
		// different provider — would not have hit.
		//
		// So the rule is not "which status codes advance" but "who refused".
		// OpenRouter marks a relayed provider failure explicitly; see
		// isProviderRelayedError. That covers 400, 401, 402 and whatever the
		// next provider invents, while a genuine account-level rejection (no
		// provider_name) still terminates as it must — a bad key is bad for
		// every model, and burning the chain would hide the one error that
		// matters.
		// 413 belongs to the same family, and more obviously so: it is a
		// statement about THIS provider's ceiling, never about the request.
		// Measured on 2026-08-14, a daily cron briefing died on:
		//   413 "Request too large for model `openai/gpt-oss-120b` ... on
		//        tokens per minute (TPM): Limit 8000, Requested 18711"
		// An ordinary agent request — tool schemas plus a short prompt — cannot
		// fit an 8k-per-minute window at all, so that provider could never have
		// served it, while the OpenRouter and Cerebras models in the same chain
		// have windows several times larger. 413 was terminal, so the chain
		// stopped there and the job had failed every day since.
		//
		// Advancing costs the same as the 400 case: at most K fast rejections
		// when the request really is too big for everyone, and the client still
		// ends up with the error.
		if resp.StatusCode == http.StatusBadRequest ||
			resp.StatusCode == http.StatusRequestEntityTooLarge ||
			isProviderRelayedError(string(respBytes)) {
			attErr.SkipModel = true
			slog.WarnContext(ctx, "[CalvoProxy] upstream rejected the request; trying the next model",
				slog.String("model", attempt.Model),
				slog.Int("status", resp.StatusCode),
				slog.Bool("provider_relayed", isProviderRelayedError(string(respBytes))))
		}
		if adapterForProvider(attempt.Provider).IsCompatibilityError(resp.StatusCode, string(respBytes)) {
			// A schema mismatch is specific to this request shape, not provider
			// health. Move to the next provider's contract without opening the
			// process-wide provider breaker for unrelated valid requests.
			attErr.SkipProvider = true
			attErr.SkipModel = true
			slog.WarnContext(ctx, "[CalvoProxy] provider rejected an unsupported request field; trying another provider",
				slog.String("provider", providerDisplayName(attempt.Provider)),
				slog.String("model", attempt.Model))
		}
		recordTraceFailure(ctx, attempt, resp.StatusCode, traceKindFor(attErr), attErr.Message)
		nonModelFailure := quotaLimited || providerAuthFailure
		if !nonModelFailure {
			s.penalizeScore(attempt, attErr.StatusCode)
		}
		if attErr.BreakerEligible && !nonModelFailure {
			// A 429/503 may carry Retry-After — respect it as a minimum cooldown.
			s.recordFailure(attempt, attErr.StatusCode, attErr.Message, providerRetryAfter)
		}
		if attErr.ProviderUnavailable {
			s.recordProviderFailure(attempt, attErr.StatusCode, attErr.Message, true, providerRetryAfter)
		} else {
			s.resolveProviderProbe(attempt.Provider)
		}
		if nonModelFailure || !attErr.BreakerEligible {
			s.resolveProbe(attempt)
		}
		return attErr
	}

	// Streaming responses (stream:true → text/event-stream) are piped straight
	// through with flushing so tokens arrive incrementally. We can't fall back
	// once bytes are on the wire, so record success on the 200 and stream.
	if isEventStream(resp) {
		s.observeAndSettleQuota(ticket, attempt, providerKey, resp.StatusCode, resp.Header, nil, estimate.Tokens, time.Now())
		settled = true
		// The model answered, so release the circuit / half-open probe now — but
		// do NOT score it a success yet: the stream still has to actually deliver.
		s.resolveProbe(attempt)
		s.resolveProviderProbe(attempt.Provider)

		// Fail fast on a queued model, BEFORE committing headers to the client.
		// A 200 with an event-stream content type only means the request was
		// accepted; upstream may still sit in a queue emitting keepalive
		// comments. Measured healthy time-to-first-event here is 0.35-0.70s,
		// while the slow tail ran 9-13s on the SAME model and prompt — variance,
		// not a broken model. Waiting it out is the single largest avoidable
		// delay in a turn.
		//
		// Skipped for the last attempt: with nothing left to fall back to,
		// abandoning converts a slow success into a fast failure.
		body := resp.Body
		if budget := streamFirstEventTimeout(); budget > 0 && !attempt.LastInChain {
			// Time-to-first-event, per model — NOT time to `s.Client.Do`
			// returning. Those measure different things: for a non-streaming
			// attempt the upstream headers arrive when generation is essentially
			// finished (tens of seconds), for a streaming one they mean only
			// "accepted" (~0.5s), so one average over both populations is
			// meaningless. Worse, a Do-level number ranks backwards for exactly
			// the failure this budget exists to catch — a model that accepts
			// instantly and then queues records a FAST sample while being
			// abandoned for slowness right here.
			firstEventStart := time.Now()
			replayed, gotEvent, timedOut := awaitFirstStreamEvent(body, budget)
			s.recordFirstEventLatency(attempt, time.Since(firstEventStart))
			if timedOut && (attempt.ReserveFallback == nil || attempt.ReserveFallback()) {
				_ = resp.Body.Close() // unblocks the reader still parked in Read
				// A soft score penalty, never a breaker failure. resolveProbe
				// above already zeroed ConsecutiveFailures, so recordFailure
				// here could never open the circuit anyway — and slowness is not
				// the same claim as brokenness. SkipModel advances the chain;
				// without it shouldChosenRetry could stop the chain outright and
				// turn a slow model into a client-visible error.
				// scoreStatusSoftOnly, not 503: applyScoreFailure escalates on
				// >=500, so passing the status we return to the client would
				// apply the HARD penalty reserved for real timeouts and rate
				// limits. Being slow to start is a weaker claim than failing.
				s.penalizeScore(attempt, scoreStatusSoftOnly)
				s.counters.streamFirstEventTimeout.Add(1)
				slog.InfoContext(ctx, "[CalvoProxy] ⏭️ abandoning queued model before first token",
					slog.String("model", attempt.Model), slog.Duration("waited", budget))
				return &attemptError{
					StatusCode: http.StatusServiceUnavailable,
					Retryable:  true,
					SkipModel:  true,
					Message:    "no stream event from " + attempt.Model + " within the first-event budget",
				}
			}
			// Not a timeout: either a real event arrived or the stream ended on
			// its own. Either way keep going with a reader that has lost nothing.
			if gotEvent {
				// A real token, so this is a time-to-first-token. A stream that
				// merely ended (gotEvent false) never produced one and must not
				// be averaged in as if it had.
				s.recordFirstTokenLatency(attempt, time.Since(attemptStart))
			}
			body = replayed
		}

		traceFrom(ctx).recordServed(attempt.Model, attempt.AttemptIndex)
		setServedModelHeaders(ctx, w, attempt)
		streamProxyResponse(w, resp)
		outcome := streamCopy(ctx, w, body, streamIdleTimeout(), streamMaxDuration())
		s.recordStreamOutcome(outcome)
		switch {
		case outcome == streamCompleted:
			s.recordSuccess(attempt) // clean EOF: a real success
			s.resolveProviderSuccess(attempt.Provider)
		case outcome.upstreamFault():
			// Stalled or died mid-stream — the model's fault. Penalise the score
			// and count it toward the breaker so a broken model stops being first.
			attErr := &attemptError{StatusCode: http.StatusBadGateway, Message: "stream ended abnormally: " + outcome.String()}
			s.penalizeScore(attempt, attErr.StatusCode)
			s.recordFailure(attempt, attErr.StatusCode, attErr.Message)
			s.resolveProviderProbe(attempt.Provider)
			slog.WarnContext(ctx, "[CalvoProxy] ⚠️ stream ended abnormally",
				slog.String("model", attempt.Model), slog.String("reason", outcome.String()))
		default:
			// Client went away, or our own max-duration backstop fired: not the
			// model's fault, so no breaker/score penalty.
			slog.InfoContext(ctx, "[CalvoProxy] stream ended without completing",
				slog.String("model", attempt.Model), slog.String("reason", outcome.String()))
		}
		// Never return an error after headers/bytes are on the wire: the fallback
		// chain cannot retry a response the client is already reading.
		return nil
	}

	// Cap the buffered non-streaming body so a huge/malformed upstream response
	// can't OOM the process. Read one byte past the limit to detect overflow.
	limit := maxResponseBytes()
	respBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if readErr != nil {
		// A truncated upstream read must not be written back as a 200. Mark it
		// retryable so the fallback chain tries the next model rather than
		// aborting (nothing was written to the client yet).
		attErr := &attemptError{StatusCode: http.StatusBadGateway, Retryable: true, Message: "truncated upstream response: " + readErr.Error()}
		s.penalizeScore(attempt, attErr.StatusCode)
		s.recordFailure(attempt, attErr.StatusCode, attErr.Message)
		s.resolveProviderProbe(attempt.Provider)
		return attErr
	}
	if int64(len(respBytes)) > limit {
		attErr := &attemptError{StatusCode: http.StatusBadGateway, Retryable: true, Message: "upstream response exceeds PROXY_MAX_RESPONSE_BYTES"}
		s.penalizeScore(attempt, attErr.StatusCode)
		s.recordFailure(attempt, attErr.StatusCode, attErr.Message)
		s.resolveProviderProbe(attempt.Provider)
		return attErr
	}
	s.observeAndSettleQuota(ticket, attempt, providerKey, resp.StatusCode, resp.Header, respBytes, actualResponseTokens(respBytes, estimate.Tokens), time.Now())
	settled = true
	s.recordSuccess(attempt)
	s.resolveProviderSuccess(attempt.Provider)
	// Only the chat-completions response shape is transformed; pass other
	// operations (e.g. Anthropic /messages) through untouched.
	if opPath == chatCompletionsPath {
		respBytes = s.transformResponse(ctx, respBytes)
	}

	traceFrom(ctx).recordServed(attempt.Model, attempt.AttemptIndex)
	setServedModelHeaders(ctx, w, attempt)
	writeProxyResponse(w, resp, respBytes)
	return nil
}

func shouldRetryAttempt(policy RetryPolicy, err *attemptError) bool {
	if err == nil {
		return false
	}
	return ShouldRetry(policy, RetryAttempt{
		StatusCode: err.StatusCode,
		Retryable:  err.Retryable,
		Timeout:    err.Timeout,
		EOF:        err.EOF,
	})
}

func waitBeforeRetry(ctx context.Context, policy RetryPolicy, attemptIndex int) {
	delay := RetryBackoff(policy, attemptIndex)
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
