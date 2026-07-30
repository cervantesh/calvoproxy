package telemetry

import (
	"context"
	"testing"
)

func TestInitBuildsTracerProvider(t *testing.T) {
	tp, err := Init("proxy-test")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if tp == nil {
		t.Fatal("expected tracer provider")
	}
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestInitCanBeDisabledFromConfig(t *testing.T) {
	t.Setenv("OTEL_ENABLED", "false")

	tp, err := Init("proxy-test")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if tp != nil {
		t.Fatal("expected disabled telemetry to return nil tracer provider")
	}
}
