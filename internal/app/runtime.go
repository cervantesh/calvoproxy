// Package app owns the process-level lifecycle for CalvoProxy.
//
// It intentionally does not know about HTTP routes, providers, or credentials.
// Those are assembled by their respective packages before the runtime starts.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Server is the small lifecycle contract shared by CalvoProxy's HTTP server
// and tests. Keeping it here avoids coupling process shutdown to a concrete
// transport implementation.
type Server interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

// Runtime owns serving and graceful shutdown after the command package has
// assembled the application's dependencies.
type Runtime struct {
	Server          Server
	ShutdownTimeout time.Duration
	Logger          *slog.Logger
}

// New creates the process runtime. The caller supplies an already assembled
// server so routing and provider policy remain outside process lifecycle code.
func New(server Server, shutdownTimeout time.Duration, logger *slog.Logger) *Runtime {
	if shutdownTimeout <= 0 {
		shutdownTimeout = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runtime{
		Server:          server,
		ShutdownTimeout: shutdownTimeout,
		Logger:          logger,
	}
}

// Run serves until the HTTP server exits or a shutdown reason arrives. It
// returns a non-nil error only when the HTTP server exits unexpectedly.
func (r *Runtime) Run(shutdown <-chan string) error {
	if r.Server == nil {
		return errors.New("app runtime requires an HTTP server")
	}

	serverErr := make(chan error, 1)
	go func() { serverErr <- r.Server.ListenAndServe() }()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case reason := <-shutdown:
		r.Logger.Info("CalvoProxy shutting down", "reason", reason)
		ctx, cancel := context.WithTimeout(context.Background(), r.ShutdownTimeout)
		defer cancel()
		if err := r.Server.Shutdown(ctx); err != nil {
			r.Logger.Warn("HTTP server shutdown failed", "error", err)
		}
		r.Logger.Info("CalvoProxy stopped")
		return nil
	}
}
