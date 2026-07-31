package router

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
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
	copyHeaders(w.Header(), resp.Header)
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
