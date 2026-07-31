package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/cervantesh/calvoproxy/internal/router"
	"github.com/cervantesh/calvoproxy/internal/telemetry"
	httpx "github.com/cervantesh/cervo-httpkit"
	"github.com/cervantesh/cervo-requestmeta"
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

func newMux(routerService *router.RouterService, idle *idleTracker) *http.ServeMux {
	mux := http.NewServeMux()

	proxyHandler := func(forcedProvider string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if idle != nil {
				idle.mark() // real proxy traffic — resets the idle-shutdown timer
			}
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

	// /health, /health/model-policy and /metrics expose internals (model chains,
	// policy hashes, upstream error text). When PROXY_ADMIN_TOKEN is set they
	// require it; otherwise they stay open (backward compatible).
	mux.HandleFunc("/health", admin(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, routerService.Health())
	}))

	mux.HandleFunc("/health/model-policy", admin(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, routerService.ModelPolicyHealth())
	}))

	mux.HandleFunc("/metrics", admin(func(w http.ResponseWriter, r *http.Request) {
		writeMetrics(w, routerService.Health())
	}))

	// /version stays open: it reports the running build and, if the startup
	// check has run, whether a newer release is available. No internals, so
	// it's safe on an exposed port.
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, currentUpdateStatus())
	})

	// /ready stays open (load-balancer probe) but returns only readiness — no
	// internals — so it's safe on an exposed port.
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		health := routerService.Health()
		w.Header().Set("Content-Type", "application/json")
		if !health.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		writeJSON(w, map[string]any{"ready": health.Ready, "status": health.Status})
	})

	// Hot-reload the model-policy.json chains without a restart. POST only, admin-gated.
	mux.HandleFunc("/admin/reload", admin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := routerService.ReloadModelPolicy(); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]any{"reloaded": false, "error": err.Error()})
			return
		}
		slog.Info("model policy reloaded via /admin/reload")
		writeJSON(w, map[string]any{"reloaded": true, "profiles": routerService.Health().Profiles})
	}))

	return mux
}

// admin gates a handler behind PROXY_ADMIN_TOKEN. When the env var is unset the
// endpoint is open (unchanged default); when set, callers must present it as a
// Bearer token or X-Admin-Token header.
func admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := os.Getenv("PROXY_ADMIN_TOKEN")
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				got = r.Header.Get("X-Admin-Token")
			}
			if got != token {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h(w, r)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// writeMetrics emits per-model reliability/breaker state in Prometheus text
// exposition format — score, consecutive failures, successes and circuit state
// per model, plus readiness and open-circuit count.
func writeMetrics(w http.ResponseWriter, h router.ProxyHealth) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP calvoproxy_up Proxy readiness (1=ready)\n# TYPE calvoproxy_up gauge\ncalvoproxy_up %d\n", boolToInt(h.Ready))
	fmt.Fprintf(w, "# HELP calvoproxy_open_circuits Open model circuits\n# TYPE calvoproxy_open_circuits gauge\ncalvoproxy_open_circuits %d\n", h.OpenCircuitCount)
	fmt.Fprintln(w, "# HELP calvoproxy_model_score Per-model reliability score [0,1]\n# TYPE calvoproxy_model_score gauge")
	for _, c := range h.Circuits {
		fmt.Fprintf(w, "calvoproxy_model_score{model=%q,state=%q} %.4f\n", c.Model, c.State, c.Score)
	}
	fmt.Fprintln(w, "# HELP calvoproxy_model_consecutive_failures Consecutive failures per model\n# TYPE calvoproxy_model_consecutive_failures gauge")
	for _, c := range h.Circuits {
		fmt.Fprintf(w, "calvoproxy_model_consecutive_failures{model=%q} %d\n", c.Model, c.ConsecutiveFailures)
	}
	fmt.Fprintln(w, "# HELP calvoproxy_model_successes Successful attempts per model\n# TYPE calvoproxy_model_successes counter")
	for _, c := range h.Circuits {
		fmt.Fprintf(w, "calvoproxy_model_successes{model=%q} %d\n", c.Model, c.Successes)
	}
}

func main() {
	// Subcommands handled before the server boots.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "update":
			os.Exit(runUpdate(os.Args[2:]))
		case "version", "--version", "-v":
			fmt.Println("CalvoProxy " + version)
			return
		}
	}
	// Remove any leftover <exe>.old from a prior Windows self-update.
	cleanupStaleUpdate()

	tp, err := telemetry.Init("CalvoProxy")
	if err != nil {
		log.Printf("Failed to initialize OpenTelemetry: %v", err)
	} else {
		defer func() {
			if err := tp.Shutdown(context.Background()); err != nil {
				log.Printf("failed to shutdown OpenTelemetry: %v", err)
			}
		}()
	}

	port := envOrDefault("PORT", "8080")
	grpcPort := envOrDefault("GRPC_PORT", "9090")
	// Bind address. Default 0.0.0.0 (needed so a container is reachable via
	// -p). For a host install set HOST=127.0.0.1 to keep the proxy — and the
	// env OpenRouter key it spends — off the network.
	host := envOrDefault("HOST", "0.0.0.0")

	routerService := router.NewRouterService()
	tracker := newIdleTracker()
	mux := newMux(routerService, tracker)

	// Best-effort background check for a newer release; logs a recommendation
	// and caches the result for GET /version. Silent for dev builds or when
	// PROXY_UPDATE_CHECK=false.
	go announceUpdate()

	// gRPC transport (unary ChatCompletion + GetHealth over the same router).
	// A cancellable context lets shutdown GracefulStop it; a bind failure is
	// non-fatal so the HTTP proxy keeps serving.
	grpcCtx, cancelGRPC := context.WithCancel(context.Background())
	if err := startGRPCServer(grpcCtx, routerService, host, grpcPort); err != nil {
		slog.Warn("gRPC server not started; continuing with HTTP only", "grpc_port", grpcPort, "error", err)
	}

	// SIGHUP → hot-reload model-policy.json without a restart (Unix; the signal
	// is never delivered on Windows, where /admin/reload is the way).
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	go func() {
		for range hupCh {
			if err := routerService.ReloadModelPolicy(); err != nil {
				slog.Warn("model policy reload (SIGHUP) failed", "error", err)
			} else {
				slog.Info("model policy reloaded (SIGHUP)")
			}
		}
	}()

	srv := httpx.NewServer(host+":"+port, mux)
	// LLM responses can be long or streamed — a fixed write deadline would cut
	// them off. Disable write/read timeouts here; ReadHeaderTimeout still
	// guards against slow-header attacks, and request bodies are bounded by
	// MaxBytesReader in the router.
	srv.WriteTimeout = 0
	srv.ReadTimeout = 0

	// Run the server; report its exit to main so we can shut down in-band and,
	// crucially, WAIT for the drain to finish before the process exits.
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe() }()

	// SIGINT/SIGTERM (e.g. `docker stop`) and idle both trigger the same drain.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	idleCh := make(chan struct{}, 1)
	startIdleShutdown(tracker, idleTimeoutFromEnv(), func() {
		select {
		case idleCh <- struct{}{}:
		default:
		}
	})

	slog.Info("CalvoProxy Smart Proxy running", "host", host, "port", port, "grpc_port", grpcPort)

	var reason string
	select {
	case err := <-srvErr:
		cancelGRPC()
		if err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
		return
	case sig := <-sigCh:
		reason = "signal:" + sig.String()
	case <-idleCh:
		reason = "idle"
	}

	slog.Info("CalvoProxy shutting down", "reason", reason)
	cancelGRPC() // GracefulStop the gRPC server
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx) // blocks until in-flight requests drain
	slog.Info("CalvoProxy stopped")
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("failed to write JSON response", slog.Any("error", err))
	}
}
