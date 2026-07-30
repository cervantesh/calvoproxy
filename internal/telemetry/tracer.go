package telemetry

import (
	cervoobserve "github.com/cervantesh/cervo-observe"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Init delegates OpenTelemetry setup to the shared CervoSoft observe package.
//
// The shared package reads OTEL_* configuration such as
// OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_INSECURE, OTEL_ENABLED, and
// OTEL_TRACES_SAMPLER_ARG. This keeps CalvoProxy aligned with Cloud Run and
// collector-based deployments without hardcoding a local Jaeger endpoint.
func Init(serviceName string) (*sdktrace.TracerProvider, error) {
	return cervoobserve.Init(serviceName)
}
