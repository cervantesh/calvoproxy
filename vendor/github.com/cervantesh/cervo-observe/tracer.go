package cervoobserve

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	configenv "github.com/cervantesh/cervo-config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Config contains OpenTelemetry setup loaded from configuration sources.
type Config struct {
	ServiceName string  `config:"OTEL_SERVICE_NAME" desc:"Service name override"`
	Endpoint    string  `config:"OTEL_EXPORTER_OTLP_ENDPOINT" default:"jaeger:4318" desc:"OTLP HTTP endpoint"`
	Insecure    bool    `config:"OTEL_EXPORTER_OTLP_INSECURE" default:"true" desc:"Whether to use insecure OTLP transport"`
	Enabled     bool    `config:"OTEL_ENABLED" default:"true" desc:"Whether tracing is enabled"`
	SampleRatio float64 `config:"OTEL_TRACES_SAMPLER_ARG" default:"1" desc:"Trace sample ratio from 0 to 1"`
}

// LoadConfig loads observability configuration.
func LoadConfig(loader *configenv.Loader, fallbackServiceName string) (Config, error) {
	if loader == nil {
		loader = configenv.New(configenv.Options{})
	}
	var cfg Config
	if err := loader.Decode(&cfg); err != nil {
		return Config{}, err
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = fallbackServiceName
	}
	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return Config{}, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG must be between 0 and 1")
	}
	return cfg, nil
}

func Init(serviceName string) (*sdktrace.TracerProvider, error) {
	cfg, err := LoadConfig(nil, serviceName)
	if err != nil {
		return nil, err
	}
	return InitWithConfig(context.Background(), cfg)
}

// InitWithConfig initializes OpenTelemetry from typed config.
func InitWithConfig(ctx context.Context, cfg Config) (*sdktrace.TracerProvider, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	res, _ := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	return tp, nil
}
