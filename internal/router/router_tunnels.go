package router

import (
	"context"
	"io"
	"log/slog"
	"net/http"
)

func (s *RouterService) tunnelToOpenRouterMessages(ctx context.Context, w http.ResponseWriter, body []byte, apiKey string) {
	proxyReq, err := newUpstreamRequest(ctx, http.MethodPost, "https://openrouter.ai/api/v1/messages", body, apiKey)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	resp, err := s.Client.Do(proxyReq)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	streamProxyResponse(w, resp)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.WarnContext(ctx, "[CervoProxy] message tunnel copy failed", slog.Any("error", err))
	}
}

func (s *RouterService) tunnelToOpenRouterEmbeddings(ctx context.Context, w http.ResponseWriter, body []byte, apiKey string) {
	proxyReq, err := newUpstreamRequest(ctx, http.MethodPost, "https://openrouter.ai/api/v1/embeddings", body, apiKey)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	resp, err := s.Client.Do(proxyReq)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	streamProxyResponse(w, resp)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.WarnContext(ctx, "[CervoProxy] embeddings tunnel copy failed", slog.Any("error", err))
	}
}
