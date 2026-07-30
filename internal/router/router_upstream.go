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
		if attErr.BreakerEligible {
			s.recordFailure(attempt, attErr.StatusCode, attErr.Message)
		}
		return attErr
	}
	defer func() { _ = resp.Body.Close() }()
	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 500 {
			span.RecordError(errors.New(string(respBytes)))
			span.SetStatus(codes.Error, "Upstream HTTP Error")
		}
		attErr := classifyHTTPError(resp.StatusCode, string(respBytes))
		if attErr.BreakerEligible {
			s.recordFailure(attempt, attErr.StatusCode, attErr.Message)
		}
		return attErr
	}

	s.recordSuccess(attempt)

	respBytes, _ := io.ReadAll(resp.Body)
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
