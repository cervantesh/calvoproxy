package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"os"
	"regexp"

	httpx "github.com/cervantesh/cervo-httpkit"
	"github.com/cervantesh/cervo-requestmeta"
	"github.com/cervoclaw/cervo-proxy/internal/router"
	"github.com/cervoclaw/cervo-proxy/internal/telemetry"
)

var profileChatPathPattern = regexp.MustCompile(`^/v1/([^/]+)/chat/completions$`)

func resolveAPIKey(r *http.Request) string {
	apiKey := requestmeta.AuthorizationFromRequest(r)
	if apiKey == "" || apiKey == "dummy" {
		envKey := os.Getenv("OPENROUTER_API_KEY")
		if envKey != "" {
			slog.Info("Using API key from environment (header was empty or dummy)")
		}
		return envKey
	}
	slog.Info("Using API key from request header")
	return apiKey
}

func requirePostAPIKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return "", false
	}

	apiKey := resolveAPIKey(r)
	if apiKey == "" {
		http.Error(w, "API Key required", http.StatusUnauthorized)
		return "", false
	}
	return apiKey, true
}

func newMux(routerService *router.RouterService) *http.ServeMux {
	mux := http.NewServeMux()

	proxyHandler := func(forcedProvider string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			apiKey, ok := requirePostAPIKey(w, r)
			if !ok {
				return
			}
			routerService.RouteRequestWithProvider(w, r, apiKey, forcedProvider)
		}
	}

	mux.HandleFunc("/chat/completions", proxyHandler(""))
	mux.HandleFunc("/v1/chat/completions", proxyHandler(""))
	mux.HandleFunc("/api/v1/chat/completions", proxyHandler(""))
	mux.HandleFunc("/v1/simple/chat/completions", proxyHandler("simple"))
	mux.HandleFunc("/v1/coding/chat/completions", proxyHandler("coding"))
	mux.HandleFunc("/v1/reasoning/chat/completions", proxyHandler("reasoning"))
	mux.HandleFunc("/v1/agent/chat/completions", proxyHandler("agent"))
	mux.HandleFunc("/v1/creative/chat/completions", proxyHandler("creative"))
	mux.HandleFunc("/v1/vision/chat/completions", proxyHandler("vision"))
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		matches := profileChatPathPattern.FindStringSubmatch(r.URL.Path)
		if len(matches) != 2 {
			http.NotFound(w, r)
			return
		}
		proxyHandler(matches[1])(w, r)
	})

	mux.HandleFunc("/messages", proxyHandler(""))
	mux.HandleFunc("/v1/messages", proxyHandler(""))
	mux.HandleFunc("/api/v1/messages", proxyHandler(""))
	mux.HandleFunc("/embeddings", proxyHandler(""))
	mux.HandleFunc("/v1/embeddings", proxyHandler(""))
	mux.HandleFunc("/api/v1/embeddings", proxyHandler(""))

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, routerService.Health())
	})

	mux.HandleFunc("/health/model-policy", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, routerService.ModelPolicyHealth())
	})

	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		health := routerService.Health()
		if !health.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		writeJSON(w, health)
	})

	return mux
}

func main() {
	tp, err := telemetry.Init("CervoProxy")
	if err != nil {
		log.Printf("Failed to initialize OpenTelemetry: %v", err)
	} else {
		defer func() {
			if err := tp.Shutdown(context.Background()); err != nil {
				log.Printf("failed to shutdown OpenTelemetry: %v", err)
			}
		}()
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	grpcPort := os.Getenv("GRPC_PORT")
	if grpcPort == "" {
		grpcPort = "9090"
	}
	host := "0.0.0.0"

	routerService := router.NewRouterService()
	handler := newIdleTracker(newMux(routerService))
	startIdleShutdown(handler, idleTimeoutFromEnv())
	if err := startGRPCServer(context.Background(), routerService, host, grpcPort); err != nil {
		log.Fatalf("failed to start gRPC server: %v", err)
	}

	slog.Info("CervoProxy Smart Proxy running", "host", host, "port", port, "grpc_port", grpcPort)
	log.Fatal(httpx.NewServer(host+":"+port, handler).ListenAndServe())
}

func writeJSON(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("failed to write JSON response", slog.Any("error", err))
	}
}
