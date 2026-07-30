# cervo-observe

`cervo-observe` provides small OpenTelemetry and structured logging helpers for
CervoSoft services. It keeps provider-specific deployment details outside the
package: applications export OTLP, while collectors, managed backends, or
platform configuration handle Google-specific authentication and routing.

## Quick Start

```go
package main

import (
	"context"
	"log"

	cervoobserve "github.com/cervantesh/cervo-observe"
)

func main() {
	tp, err := cervoobserve.Init("calvoproxy-api")
	if err != nil {
		log.Fatal(err)
	}
	if tp != nil {
		defer tp.Shutdown(context.Background())
	}
}
```

## Configuration

`LoadConfig` reads these variables through `cervo-config`:

| Variable | Default | Description |
| --- | --- | --- |
| `OTEL_SERVICE_NAME` | fallback service name | Service name used in trace resources. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `jaeger:4318` | OTLP HTTP endpoint. |
| `OTEL_EXPORTER_OTLP_INSECURE` | `true` | Use insecure OTLP HTTP transport. |
| `OTEL_ENABLED` | `true` | Disable tracing when set to false. |
| `OTEL_TRACES_SAMPLER_ARG` | `1` | Sampling ratio from 0 to 1. |

## Local Collector

For local development with an OTLP HTTP collector:

```powershell
$env:OTEL_SERVICE_NAME = "calvoproxy-api"
$env:OTEL_EXPORTER_OTLP_ENDPOINT = "localhost:4318"
$env:OTEL_EXPORTER_OTLP_INSECURE = "true"
$env:OTEL_ENABLED = "true"
```

## Cloud Run

For Cloud Run, set service identity and endpoint through deployment
configuration:

```powershell
$env:OTEL_SERVICE_NAME = "calvoproxy-api"
$env:OTEL_EXPORTER_OTLP_ENDPOINT = "otel-collector.internal:4318"
$env:OTEL_EXPORTER_OTLP_INSECURE = "false"
$env:OTEL_ENABLED = "true"
$env:OTEL_TRACES_SAMPLER_ARG = "0.25"
```

Recommended deployment pattern:

1. CervoClaw service exports OTLP HTTP.
2. A collector or managed backend handles Google authentication/export.
3. Cloud Run service name, revision, and environment are provided through
   deployment metadata or environment variables.
4. No Google SDK is required in this package.

## Propagation

`InitWithConfig` installs W3C trace context and baggage propagators globally.
HTTP middleware, event publishers, and tool-call clients should carry those
headers or metadata across service boundaries so CervoClaw requests, events,
and agent actions remain correlated.

## Sensitive Values

Use `RedactMap` before logging configuration maps or request metadata. Keys
containing token, secret, password, API key, authorization, or cookie are
redacted. Use `Fingerprint` when an operator needs to compare secret identity
without printing the secret itself.

## Development

```bash
go test ./...
```
