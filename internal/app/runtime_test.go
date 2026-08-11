package app

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type fakeServer struct {
	listenErr   chan error
	shutdowns   atomic.Int32
	shutdownErr error
}

func (s *fakeServer) ListenAndServe() error { return <-s.listenErr }

func (s *fakeServer) Shutdown(context.Context) error {
	s.shutdowns.Add(1)
	return s.shutdownErr
}

func TestRuntimeReturnsUnexpectedServerFailure(t *testing.T) {
	server := &fakeServer{listenErr: make(chan error, 1)}
	server.listenErr <- errors.New("bind failed")
	var grpcStops atomic.Int32

	err := New(server, func() { grpcStops.Add(1) }, time.Second, nil).Run(make(chan string))
	if err == nil || err.Error() != "bind failed" {
		t.Fatalf("Run() error = %v, want bind failure", err)
	}
	if got := server.shutdowns.Load(); got != 0 {
		t.Fatalf("Shutdown() calls = %d, want 0", got)
	}
	if got := grpcStops.Load(); got != 1 {
		t.Fatalf("gRPC stop calls = %d, want 1", got)
	}
}

func TestRuntimeGracefullyStopsHTTPAndGRPC(t *testing.T) {
	server := &fakeServer{listenErr: make(chan error)}
	shutdown := make(chan string, 1)
	shutdown <- "signal:interrupt"
	var grpcStops atomic.Int32

	err := New(server, func() { grpcStops.Add(1) }, time.Second, nil).Run(shutdown)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got := server.shutdowns.Load(); got != 1 {
		t.Fatalf("Shutdown() calls = %d, want 1", got)
	}
	if got := grpcStops.Load(); got != 1 {
		t.Fatalf("gRPC stop calls = %d, want 1", got)
	}
}

func TestRuntimeAcceptsNormalServerClose(t *testing.T) {
	server := &fakeServer{listenErr: make(chan error, 1)}
	server.listenErr <- http.ErrServerClosed

	if err := New(server, nil, time.Second, nil).Run(make(chan string)); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
}
