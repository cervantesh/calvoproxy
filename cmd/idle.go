package main

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// idleTracker wraps the HTTP handler and records the time of the last "real"
// request (actual proxy traffic, not health/readiness probes) so the process
// can self-terminate after a period of inactivity. This is what makes CalvoProxy
// safe to run on-demand: a launcher starts it when first needed, and it exits
// once traffic stops.
type idleTracker struct {
	next http.Handler
	last atomic.Int64 // UnixNano of last non-probe request
}

func newIdleTracker(next http.Handler) *idleTracker {
	t := &idleTracker{next: next}
	t.last.Store(time.Now().UnixNano())
	return t
}

// isProbe reports whether a path is a health/readiness probe that must NOT
// count as activity — otherwise a monitor polling /health would keep the
// process alive forever and defeat idle shutdown.
func isProbe(path string) bool {
	switch path {
	case "/health", "/ready", "/health/model-policy":
		return true
	default:
		return false
	}
}

func (t *idleTracker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isProbe(r.URL.Path) {
		t.last.Store(time.Now().UnixNano())
	}
	t.next.ServeHTTP(w, r)
}

func (t *idleTracker) idleFor() time.Duration {
	return time.Since(time.Unix(0, t.last.Load()))
}

// startIdleShutdown exits the process once no non-probe request has arrived
// within `timeout`. A zero/negative timeout disables the watchdog (the proxy
// then runs until killed, its original always-on behaviour).
func startIdleShutdown(t *idleTracker, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	slog.Info("CalvoProxy idle shutdown armed", "timeout", timeout.String())
	go func() {
		interval := timeout / 4
		if interval < 5*time.Second {
			interval = 5 * time.Second
		}
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for range tick.C {
			if idle := t.idleFor(); idle >= timeout {
				slog.Info("CalvoProxy exiting after idle period", "idle", idle.Round(time.Second).String(), "timeout", timeout.String())
				os.Exit(0)
			}
		}
	}()
}

// idleTimeoutFromEnv parses PROXY_IDLE_TIMEOUT (Go duration, e.g. "20m").
// Empty or invalid → 0 (disabled).
func idleTimeoutFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("PROXY_IDLE_TIMEOUT"))
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Warn("invalid PROXY_IDLE_TIMEOUT; ignoring", "value", raw, "error", err)
		return 0
	}
	return d
}
