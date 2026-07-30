package main

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// idleTracker records the time of the last *real* proxy request so the process
// can self-terminate after a period of inactivity. This is what makes CalvoProxy
// safe to run on-demand: a launcher starts it when first needed, and it exits
// once traffic stops.
//
// Only genuine proxy traffic calls mark(). Health/readiness probes and unknown
// routes (stray pollers or scanners sharing the port — e.g. a kubectl context
// pointed at localhost:8080 hitting /api, or a monitor polling /) do NOT count
// as activity, so they cannot keep the process alive and defeat idle shutdown.
type idleTracker struct {
	last atomic.Int64 // UnixNano of last real proxy request
}

func newIdleTracker() *idleTracker {
	t := &idleTracker{}
	t.last.Store(time.Now().UnixNano())
	return t
}

// mark records activity. Called by the proxy request handlers (chat/messages/
// embeddings), not by probe or unmatched routes.
func (t *idleTracker) mark() { t.last.Store(time.Now().UnixNano()) }

func (t *idleTracker) idleFor() time.Duration {
	return time.Since(time.Unix(0, t.last.Load()))
}

// startIdleShutdown exits the process once no real proxy request has arrived
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
