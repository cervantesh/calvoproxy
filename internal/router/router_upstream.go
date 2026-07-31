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

	target := s.resolveAttemptTarget(attempt, chatCompletionsPath)
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
		s.recordSuccess(attempt)
		streamProxyResponse(w, resp)
		streamCopy(ctx, w, resp.Body, streamIdleTimeout(), streamMaxDuration())
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
	respBytes = s.transformResponse(ctx, respBytes)

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
