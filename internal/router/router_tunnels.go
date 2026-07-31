package router

import (
	"context"
	"net/http"
)

func (s *RouterService) tunnelToOpenRouterMessages(ctx context.Context, w http.ResponseWriter, body []byte, apiKey string) {
	proxyReq, err := newUpstreamRequest(ctx, http.MethodPost, openRouterMessagesURL(), body, apiKey)
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
	streamCopy(ctx, w, resp.Body, streamIdleTimeout(), streamMaxDuration())
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
	streamCopy(ctx, w, resp.Body, streamIdleTimeout(), streamMaxDuration())
}
