package router

import (
	"context"
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

	// Single-flight the half-open recovery probe: if this model's circuit just
	// entered its half-open window and another request already claimed the probe,
	// skip to the next model instead of stampeding the recovering upstream. This
	// is a soft skip (retryable, not breaker-eligible, no score penalty).
	if !s.tryStartAttempt(attempt) {
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

	proxyReq, err := newUpstreamRequest(ctx, http.MethodPost, target.URL, body, apiKey)
	if err != nil {
		return classifyTransportError(err)
	}

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
			return &attemptError{
				StatusCode: http.StatusServiceUnavailable,
				Retryable:  false,
				SkipModel:  true,
				Message:    "request cancelled before the upstream responded",
			}
		}
		attErr := classifyTransportError(err)
		s.penalizeScore(attempt, attErr.StatusCode)
		if attErr.BreakerEligible {
			s.recordFailure(attempt, attErr.StatusCode, attErr.Message)
		}
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
		s.penalizeScore(attempt, attErr.StatusCode)
		if attErr.BreakerEligible {
			// A 429/503 may carry Retry-After — respect it as a minimum cooldown.
			s.recordFailure(attempt, attErr.StatusCode, attErr.Message, parseRetryAfter(resp.Header.Get("Retry-After")))
		}
		return attErr
	}

	// Streaming responses (stream:true → text/event-stream) are piped straight
	// through with flushing so tokens arrive incrementally. We can't fall back
	// once bytes are on the wire, so record success on the 200 and stream.
	if isEventStream(resp) {
		// The model answered, so release the circuit / half-open probe now — but
		// do NOT score it a success yet: the stream still has to actually deliver.
		s.resolveProbe(attempt)
		streamProxyResponse(w, resp)
		outcome := streamCopy(ctx, w, resp.Body, streamIdleTimeout(), streamMaxDuration())
		s.recordStreamOutcome(outcome)
		switch {
		case outcome == streamCompleted:
			s.recordSuccess(attempt) // clean EOF: a real success
		case outcome.upstreamFault():
			// Stalled or died mid-stream — the model's fault. Penalise the score
			// and count it toward the breaker so a broken model stops being first.
			attErr := &attemptError{StatusCode: http.StatusBadGateway, Message: "stream ended abnormally: " + outcome.String()}
			s.penalizeScore(attempt, attErr.StatusCode)
			s.recordFailure(attempt, attErr.StatusCode, attErr.Message)
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
		return attErr
	}
	if int64(len(respBytes)) > limit {
		attErr := &attemptError{StatusCode: http.StatusBadGateway, Retryable: true, Message: "upstream response exceeds PROXY_MAX_RESPONSE_BYTES"}
		s.penalizeScore(attempt, attErr.StatusCode)
		return attErr
	}
	s.recordSuccess(attempt)
	// Only the chat-completions response shape is transformed; pass other
	// operations (e.g. Anthropic /messages) through untouched.
	if opPath == chatCompletionsPath {
		respBytes = s.transformResponse(ctx, respBytes)
	}

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
