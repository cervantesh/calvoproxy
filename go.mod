module github.com/cervantesh/calvoproxy

go 1.26.5

require (
	github.com/cervantesh/cervo-compress v0.0.0-20260805105237-61a3f522f7e8
	github.com/cervantesh/cervo-contracts v0.0.0-00010101000000-000000000000
	github.com/cervantesh/cervo-requestmeta v0.0.0-00010101000000-000000000000
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/sdk v1.45.0
)

require (
	github.com/cervantesh/cervo-config v0.3.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.45.0 // indirect
)

require (
	github.com/cervantesh/cervo-httpkit v0.0.0
	github.com/cervantesh/cervo-model-policy v0.0.0
	github.com/cervantesh/cervo-observe v0.0.0
	github.com/cervantesh/cervo-retry v0.0.0
	github.com/cervantesh/cervo-rules/v3 v3.0.0-rc.6
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0
	go.opentelemetry.io/otel/trace v1.45.0
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/grpc v1.83.0
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/cervantesh/cervo-claw-events => ./third_party/cervo-claw-events

replace github.com/cervantesh/cervo-config => ./third_party/cervo-config

replace github.com/cervantesh/cervo-contracts => ./third_party/cervo-contracts

replace github.com/cervantesh/cervo-eventbus => ./third_party/cervo-eventbus

replace github.com/cervantesh/cervo-health => ./third_party/cervo-health

replace github.com/cervantesh/cervo-httpkit => ./third_party/cervo-httpkit

replace github.com/cervantesh/cervo-llmclient => ./third_party/cervo-llmclient

replace github.com/cervantesh/cervo-model-policy => ./third_party/cervo-model-policy

replace github.com/cervantesh/cervo-mutant => ./third_party/cervo-mutant

replace github.com/cervantesh/cervo-observe => ./third_party/cervo-observe

replace github.com/cervantesh/cervo-redisconfig => ./third_party/cervo-redisconfig

replace github.com/cervantesh/cervo-requestmeta => ./third_party/cervo-requestmeta

replace github.com/cervantesh/cervo-retry => ./third_party/cervo-retry

replace github.com/cervantesh/cervo-rules/v3 => ./third_party/cervo-rules

