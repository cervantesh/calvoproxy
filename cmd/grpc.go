package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/cervantesh/cervo-requestmeta"
	"github.com/cervoclaw/cervo-proxy/internal/router"
	proxyv1 "github.com/cervoclaw/cervo-shared/gen/proto/cervoclaw/proxy/v1"
	"google.golang.org/grpc"
)

type proxyTransportGRPCServer struct {
	proxyv1.UnimplementedProxyTransportServiceServer
	routerService *routerServiceAdapter
}

type routerServiceAdapter struct {
	routeRequestWithProvider func(http.ResponseWriter, *http.Request, string, string)
	health                   func() interface{}
}

func newProxyTransportGRPCServer(routerService *router.RouterService) *proxyTransportGRPCServer {
	return &proxyTransportGRPCServer{
		routerService: &routerServiceAdapter{
			routeRequestWithProvider: routerService.RouteRequestWithProvider,
			health: func() interface{} {
				return routerService.Health()
			},
		},
	}
}

func (s *proxyTransportGRPCServer) ChatCompletion(ctx context.Context, req *proxyv1.ChatCompletionRequest) (*proxyv1.ChatCompletionResponse, error) {
	path := strings.TrimSpace(req.GetPath())
	if path == "" {
		path = "/v1/chat/completions"
	}
	httpReq := httptest.NewRequest(http.MethodPost, path, strings.NewReader(req.GetBodyJson())).WithContext(ctx)
	httpReq.Header.Set("Content-Type", "application/json")
	if auth := strings.TrimSpace(req.GetAuthorization()); auth != "" {
		httpReq.Header.Set(requestmeta.HeaderAuthorization, auth)
	}
	for key, value := range req.GetHeaders() {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		httpReq.Header.Set(key, value)
	}

	recorder := httptest.NewRecorder()
	apiKey := resolveAPIKey(httpReq)
	if apiKey == "" {
		return &proxyv1.ChatCompletionResponse{
			StatusCode: http.StatusUnauthorized,
			BodyJson:   "API Key required\n",
			Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		}, nil
	}
	s.routerService.routeRequestWithProvider(recorder, httpReq, apiKey, strings.TrimSpace(req.GetProvider()))

	headers := map[string]string{}
	for key, values := range recorder.Header() {
		if len(values) == 0 {
			continue
		}
		headers[key] = values[0]
	}
	return &proxyv1.ChatCompletionResponse{
		StatusCode: int32(recorder.Code),
		BodyJson:   recorder.Body.String(),
		Headers:    headers,
	}, nil
}

func (s *proxyTransportGRPCServer) GetHealth(ctx context.Context, req *proxyv1.GetHealthRequest) (*proxyv1.GetHealthResponse, error) {
	payload, err := json.Marshal(s.routerService.health())
	if err != nil {
		return nil, err
	}
	return &proxyv1.GetHealthResponse{BodyJson: string(payload)}, nil
}

func startGRPCServer(ctx context.Context, routerService *router.RouterService, host, port string) error {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	server := grpc.NewServer()
	proxyv1.RegisterProxyTransportServiceServer(server, newProxyTransportGRPCServer(routerService))
	go func() {
		<-ctx.Done()
		server.GracefulStop()
		_ = listener.Close()
	}()
	go func() {
		if err := server.Serve(listener); err != nil && ctx.Err() == nil {
			slog.Error("gRPC server stopped unexpectedly", "error", err)
		}
	}()
	return nil
}
