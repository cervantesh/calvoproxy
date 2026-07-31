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
		// Don't silently spend the env OpenRouter key for a keyless request when
		// bound to a public interface — that turns an exposed instance into an
		// open relay on someone else's dime. Loopback binds keep the old
		// behaviour; a public bind requires PROXY_ALLOW_ENV_KEY_PUBLIC=true.
		if boundToPublicInterface() && !allowEnvKeyOnPublicBind() {
			slog.Warn("Refusing env OPENROUTER_API_KEY for a keyless request on a public bind; set PROXY_ALLOW_ENV_KEY_PUBLIC=true to allow, or pass a key")
			return ""
		}
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
			// Wrap to capture status + latency for /metrics. The recorder forwards
			// http.Flusher so streaming still flushes token-by-token.
			rec := newStatusRecorder(w)
			start := time.Now()
			defer func() { metrics.observe(rec.status, time.Since(start).Nanoseconds()) }()
			apiKey, ok := requirePostAPIKey(rec, r)
			if !ok {
				return
			}
			routerService.RouteRequestWithProvider(rec, r, apiKey, forcedProvider)
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

	mux.HandleFunc("/metrics", metricsAuth(func(w http.ResponseWriter, r *http.Request) {
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

// presentedToken extracts the caller's token from a Bearer Authorization header
// or the X-Admin-Token header.
func presentedToken(r *http.Request) string {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.Header.Get("X-Admin-Token")
	}
	return got
}

// admin gates a handler behind PROXY_ADMIN_TOKEN. When the env var is unset the
// endpoint is open (unchanged default); when set, callers must present it as a
// Bearer token or X-Admin-Token header. The comparison is constant-time.
func admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := os.Getenv("PROXY_ADMIN_TOKEN")
		if token != "" && !constantTimeEqual(presentedToken(r), token) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// metricsAuth gates /metrics. If PROXY_METRICS_TOKEN is set, /metrics accepts it
// OR the admin token, decoupling a Prometheus scraper's credential from the
// admin credential. If it is unset, /metrics follows the admin gate as before.
func metricsAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		metricsToken := os.Getenv("PROXY_METRICS_TOKEN")
		if metricsToken == "" {
			admin(h)(w, r)
			return
		}
		got := presentedToken(r)
		adminToken := os.Getenv("PROXY_ADMIN_TOKEN")
		if constantTimeEqual(got, metricsToken) || (adminToken != "" && constantTimeEqual(got, adminToken)) {
			h(w, r)
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
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

	// Request-level counters an operator alerts on: rate, error classes, latency.
	fmt.Fprintln(w, "# HELP calvoproxy_requests_total Proxy requests handled\n# TYPE calvoproxy_requests_total counter")
	fmt.Fprintf(w, "calvoproxy_requests_total %d\n", metrics.requestsTotal.Load())
	fmt.Fprintln(w, "# HELP calvoproxy_requests_by_status Proxy requests by HTTP status class\n# TYPE calvoproxy_requests_by_status counter")
	fmt.Fprintf(w, "calvoproxy_requests_by_status{class=\"2xx\"} %d\n", metrics.status2xx.Load())
	fmt.Fprintf(w, "calvoproxy_requests_by_status{class=\"4xx\"} %d\n", metrics.status4xx.Load())
	fmt.Fprintf(w, "calvoproxy_requests_by_status{class=\"5xx\"} %d\n", metrics.status5xx.Load())
	fmt.Fprintf(w, "calvoproxy_requests_by_status{class=\"other\"} %d\n", metrics.statusOther.Load())
	fmt.Fprintln(w, "# HELP calvoproxy_request_latency_seconds_sum Total handler latency\n# TYPE calvoproxy_request_latency_seconds_sum counter")
	fmt.Fprintf(w, "calvoproxy_request_latency_seconds_sum %.6f\n", float64(metrics.latencyNanos.Load())/1e9)
	fmt.Fprintln(w, "# HELP calvoproxy_request_latency_count Handler latency observations\n# TYPE calvoproxy_request_latency_count counter")
	fmt.Fprintf(w, "calvoproxy_request_latency_count %d\n", metrics.latencyCount.Load())
	fmt.Fprintln(w, "# HELP calvoproxy_build_info Build version (value always 1)\n# TYPE calvoproxy_build_info gauge")
	fmt.Fprintf(w, "calvoproxy_build_info{version=%q} 1\n", version)
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

	// Registered first so it runs LAST (after telemetry flush) — lets us exit
	// non-zero on a fatal server error without a bare os.Exit skipping defers.
	exitCode := 0
	defer func() { os.Exit(exitCode) }()

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
	// Bind address. Defaults to loopback (127.0.0.1) so a host install keeps the
	// proxy — and the env OpenRouter key it spends — off the network by default.
	// The Docker image sets HOST=0.0.0.0 so a container stays reachable via -p.
	host := envOrDefault("HOST", "127.0.0.1")
	bindHost = host

	// Loud warning when exposed on a public interface without an admin token:
	// /health, /metrics and /admin/reload are open by default and leak internals.
	if boundToPublicInterface() && os.Getenv("PROXY_ADMIN_TOKEN") == "" {
		slog.Warn("CalvoProxy is bound to a PUBLIC interface with no PROXY_ADMIN_TOKEN — /health, /metrics and /admin/reload are open. Set PROXY_ADMIN_TOKEN, or bind HOST=127.0.0.1.",
			"host", host)
	}

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
			// Return (don't log.Fatal) so the deferred telemetry/OTel shutdown
			// still runs and flushes before the process exits.
			slog.Error("HTTP server exited with error", "error", err)
			exitCode = 1
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
