package main

import (
	"context"
	"net/http"
	"testing"

	proxyv1 "github.com/cervantesh/calvoproxy/gen/proto/proxyv1"
	"github.com/cervantesh/calvoproxy/internal/secretstore"
)

func TestProxyTransportGRPCServerChatCompletion(t *testing.T) {
	server := &proxyTransportGRPCServer{
		routerService: &routerServiceAdapter{
			routeRequestWithProvider: func(w http.ResponseWriter, r *http.Request, apiKey string, provider string) {
				if provider != "agent" {
					t.Fatalf("expected provider propagated, got %q", provider)
				}
				if apiKey != "test-token" {
					t.Fatalf("expected api key from auth header, got %q", apiKey)
				}
				if got := r.URL.Path; got != "/v1/chat/completions" {
					t.Fatalf("unexpected path: %s", got)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"ok":true}`))
			},
			health: func() interface{} { return map[string]any{"ready": true} },
		},
	}

	resp, err := server.ChatCompletion(context.Background(), &proxyv1.ChatCompletionRequest{
		Path:          "/v1/chat/completions",
		Provider:      "agent",
		Authorization: "Bearer test-token",
		BodyJson:      `{"messages":[]}`,
	})
	if err != nil {
		t.Fatalf("ChatCompletion error: %v", err)
	}
	if got := resp.GetStatusCode(); got != http.StatusAccepted {
		t.Fatalf("unexpected status: %d", got)
	}
	if got := resp.GetBodyJson(); got != `{"ok":true}` {
		t.Fatalf("unexpected body: %s", got)
	}
	if got := resp.GetHeaders()["Content-Type"]; got != "application/json" {
		t.Fatalf("unexpected content-type: %q", got)
	}
}

func TestProxyTransportGRPCServerHealth(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "")
	server := &proxyTransportGRPCServer{
		routerService: &routerServiceAdapter{
			routeRequestWithProvider: func(http.ResponseWriter, *http.Request, string, string) {},
			health:                   func() interface{} { return map[string]any{"ready": true} },
		},
	}
	resp, err := server.GetHealth(context.Background(), &proxyv1.GetHealthRequest{})
	if err != nil {
		t.Fatalf("GetHealth error: %v", err)
	}
	if got := resp.GetBodyJson(); got != `{"ready":true}` {
		t.Fatalf("unexpected health body: %s", got)
	}
}

func TestProxyTransportGRPCServerRequiresAPIKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("CEREBRAS_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	server := &proxyTransportGRPCServer{
		routerService: &routerServiceAdapter{
			routeRequestWithProvider: func(http.ResponseWriter, *http.Request, string, string) {
				t.Fatal("router should not be called without api key")
			},
			health: func() interface{} { return map[string]any{"ready": true} },
		},
	}
	resp, err := server.ChatCompletion(context.Background(), &proxyv1.ChatCompletionRequest{
		Path:     "/v1/chat/completions",
		BodyJson: `{"messages":[]}`,
	})
	if err != nil {
		t.Fatalf("ChatCompletion error: %v", err)
	}
	if got := resp.GetStatusCode(); got != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", got)
	}
}

func TestProxyTransportGRPCServerAllowsManagedDirectProviderCredential(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("CEREBRAS_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	useManagedCredentialStore(t, &memoryCredentialStore{values: map[secretstore.Provider][]byte{
		secretstore.ProviderCerebras: []byte("managed-cerebras"),
	}})
	oldBindHost := bindHost
	bindHost = "127.0.0.1"
	t.Cleanup(func() { bindHost = oldBindHost })

	called := false
	server := &proxyTransportGRPCServer{routerService: &routerServiceAdapter{
		routeRequestWithProvider: func(w http.ResponseWriter, r *http.Request, apiKey string, _ string) {
			called = true
			if apiKey != "" {
				t.Fatalf("direct-provider-only request received OpenRouter key %q", apiKey)
			}
			w.WriteHeader(http.StatusOK)
		},
		health: func() interface{} { return map[string]any{"ready": true} },
	}}
	resp, err := server.ChatCompletion(context.Background(), &proxyv1.ChatCompletionRequest{BodyJson: `{"messages":[]}`})
	if err != nil || resp.GetStatusCode() != http.StatusOK || !called {
		t.Fatalf("managed direct provider was not admitted: status=%d called=%v err=%v", resp.GetStatusCode(), called, err)
	}
}

func TestProxyTransportGRPCServerCarriesRouteTrace(t *testing.T) {
	server := &proxyTransportGRPCServer{routerService: &routerServiceAdapter{
		routeRequestWithProvider: func(w http.ResponseWriter, _ *http.Request, _ string, _ string) {
			w.Header().Set("X-Calvoproxy-Route", "v1;p=coding;cmp=off")
			w.Header().Set("X-Calvoproxy-Decision-Id", "0123456789abcdef")
			w.WriteHeader(http.StatusOK)
		},
		health: func() interface{} { return map[string]any{"ready": true} },
	}}
	resp, err := server.ChatCompletion(context.Background(), &proxyv1.ChatCompletionRequest{
		Authorization: "Bearer test-token",
		BodyJson:      `{"messages":[]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetHeaders()["X-Calvoproxy-Route"]; got != "v1;p=coding;cmp=off" {
		t.Errorf("route trace lost over gRPC: %q", got)
	}
	if got := resp.GetHeaders()["X-Calvoproxy-Decision-Id"]; got != "0123456789abcdef" {
		t.Errorf("decision id lost over gRPC: %q", got)
	}
}
