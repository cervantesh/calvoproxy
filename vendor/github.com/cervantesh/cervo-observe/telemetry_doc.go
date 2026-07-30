// Package telemetry initializes shared OpenTelemetry tracing defaults.
//
// Extraction boundary: telemetry should own observability bootstrap only and
// must not import application services.
package cervoobserve
