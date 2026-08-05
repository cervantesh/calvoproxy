package router

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

const proxyTitleHeader = "CalvoProxy Smart Proxy"

func newUpstreamRequest(ctx context.Context, method string, targetURL string, body []byte, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	applyProxyHeaders(req, apiKey)
	return req, nil
}

func applyProxyHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Title", proxyTitleHeader)
	otel.GetTextMapPropagator().Inject(req.Context(), propagation.HeaderCarrier(req.Header))
}

func writeProxyResponse(w http.ResponseWriter, resp *http.Response, body []byte) {
	// Drop Content-Encoding along with Content-Length: `body` is the already
	// decoded (and possibly transformed) payload, so forwarding an upstream
	// encoding header would tell the client to decompress plain bytes.
	copyHeaders(w.Header(), resp.Header, "content-length", "content-encoding")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func streamProxyResponse(w http.ResponseWriter, resp *http.Response) {
	// Skip our own trace headers: copyHeaders Adds, and it runs AFTER
	// setServedModelHeaders, so an upstream echoing them would leave the client
	// with two values — the second one attacker-controlled text presented as this
	// proxy's routing decision. Verified: the observed pair was
	// ["v1;p=simple;cmp=off", "v1;p=INJECTED"].
	copyHeaders(w.Header(), resp.Header, traceHeader, traceIDHeader)
	w.WriteHeader(resp.StatusCode)
}

func copyHeaders(dst http.Header, src http.Header, skipKeys ...string) {
	skip := map[string]struct{}{}
	for _, key := range skipKeys {
		skip[strings.ToLower(key)] = struct{}{}
	}
	for key, values := range src {
		if _, ok := skip[strings.ToLower(key)]; ok {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// setServedModelHeaders tells the caller which model actually answered.
//
// A profile name is a request, not a promise: the chain reorders by reliability
// and falls through on failure, so "coding" can be served by any member. Without
// this the caller cannot tell a first-choice answer from a fallback — the body's
// `model` field carries the id, but only for callers that parse the body, and
// not at all for a client that only reads headers.
//
// X-Calvoproxy-Attempt > 1 is the degraded signal. It exists because a chain
// that degrades silently produced a real incident: a design review answered by
// the third model in the chain, believed to be the first.
//
// Must be called BEFORE the response headers are written.
//
// It also materialises the route trace (X-Calvoproxy-Route), which answers the
// question the headers above cannot: not which model answered, but why that one.
// Same ordering requirement, and the same single call site for both — see
// docs/specs/P1-decision-trace.md.
func setServedModelHeaders(ctx context.Context, w http.ResponseWriter, attempt modelAttempt) {
	h := w.Header()
	h.Set("X-Calvoproxy-Model", attempt.Model)
	h.Set("X-Calvoproxy-Profile", attempt.Profile)
	if attempt.AttemptIndex > 0 {
		h.Set("X-Calvoproxy-Attempt", strconv.Itoa(attempt.AttemptIndex))
	}
	setRouteTraceHeaders(h, traceFrom(ctx))
}

func setRouteTraceHeaders(h http.Header, trace *routeTrace) {
	if trace == nil {
		return
	}
	h.Set(traceHeader, trace.header())
	if trace.ID != "" {
		h.Set(traceIDHeader, trace.ID)
	}
}
