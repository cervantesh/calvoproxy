package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	proxyv1 "github.com/cervantesh/calvoproxy/gen/proto/proxyv1"
	"github.com/cervantesh/calvoproxy/internal/router"
	"github.com/cervantesh/cervo-requestmeta"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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

// requestsStreaming reports whether a chat body explicitly asks for streaming.
// Only an explicit boolean true counts (matching how the router itself reads it).
func requestsStreaming(bodyJSON string) bool {
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(bodyJSON), &body); err != nil {
		return false
	}
	stream, _ := body["stream"].(bool)
	return stream
}

func (s *proxyTransportGRPCServer) ChatCompletion(ctx context.Context, req *proxyv1.ChatCompletionRequest) (*proxyv1.ChatCompletionResponse, error) {
	// This unary RPC runs the router against an in-memory recorder, so a streamed
	// response would be buffered whole (potentially many minutes of tokens) and
	// delivered as one blob — no flushing, unbounded memory, and nothing a
	// streaming client can consume. Refuse explicitly rather than silently
	// downgrading, which would leave SSE consumers parsing something unexpected.
	if requestsStreaming(req.GetBodyJson()) {
		return nil, status.Error(codes.InvalidArgument,
			"streaming is not supported on the gRPC unary transport; use the HTTP API (POST /v1/chat/completions) for stream:true")
	}
	start := time.Now()
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
		metrics.observe(http.StatusUnauthorized, time.Since(start).Nanoseconds())
		metrics.grpcRequests.Add(1)
		return &proxyv1.ChatCompletionResponse{
			StatusCode: http.StatusUnauthorized,
			BodyJson:   "API Key required\n",
			Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		}, nil
	}
	s.routerService.routeRequestWithProvider(recorder, httpReq, apiKey, strings.TrimSpace(req.GetProvider()))
	// gRPC traffic used to be invisible in /metrics (only the HTTP handler
	// recorded); count it the same way so operators see all proxy load.
	metrics.observe(recorder.Code, time.Since(start).Nanoseconds())
	metrics.grpcRequests.Add(1)

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

// grpcAdminAuthorized mirrors the HTTP admin gate for gRPC: when
// PROXY_ADMIN_TOKEN is set, the caller must present it as gRPC metadata
// "authorization: Bearer <token>" or "x-admin-token: <token>" (constant-time).
// When unset, the endpoint is open (unchanged default).
func grpcAdminAuthorized(ctx context.Context) bool {
	token := os.Getenv("PROXY_ADMIN_TOKEN")
	if token == "" {
		return true
	}
	md, _ := metadata.FromIncomingContext(ctx)
	got := ""
	if vals := md.Get("authorization"); len(vals) > 0 {
		got = strings.TrimPrefix(vals[0], "Bearer ")
	}
	if got == "" {
		if vals := md.Get("x-admin-token"); len(vals) > 0 {
			got = vals[0]
		}
	}
	return constantTimeEqual(got, token)
}

func (s *proxyTransportGRPCServer) GetHealth(ctx context.Context, req *proxyv1.GetHealthRequest) (*proxyv1.GetHealthResponse, error) {
	// Health leaks internals (model chains, breaker reasons, policy hashes) — gate
	// it behind the admin token, same as HTTP /health.
	if !grpcAdminAuthorized(ctx) {
		return nil, status.Error(codes.Unauthenticated, "admin token required")
	}
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
