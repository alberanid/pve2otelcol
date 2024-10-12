package ologgers

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

type OLogger struct {
	Logger otellog.Logger
	Ctx    context.Context
}

func (o *OLogger) LogRecord(r otellog.Record) {
	o.Logger.Emit(o.Ctx, r)
}

type OLoggerOptions struct {
	Endpoint    string
	ServiceName string
	LoggerName  string
}

func New(opts OLoggerOptions) (*OLogger, error) {
	if opts.Endpoint == "" {
		opts.Endpoint = "http://localhost:4317"
	}
	if opts.LoggerName == "" {
		opts.LoggerName = "pve2otelcol"
	}
	ctx := context.Background()
	rpcOptions := []otlploggrpc.Option{
		otlploggrpc.WithEndpointURL(opts.Endpoint),
		otlploggrpc.WithCompressor("gzip"),
		otlploggrpc.WithReconnectionPeriod(time.Duration(time.Duration(10) * time.Second)),
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: time.Duration(2) * time.Second,
			MaxInterval:     time.Duration(10) * time.Second,
			MaxElapsedTime:  time.Duration(30) * time.Second,
		}),
	}
	exporter, err := otlploggrpc.New(ctx, rpcOptions...)
	if err != nil {
		return nil, err
	}

	providerResources, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(opts.ServiceName),
		),
	)
	if err != nil {
		return nil, err
	}

	processor := sdklog.NewBatchProcessor(exporter)
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(processor),
		sdklog.WithResource(providerResources),
	)
	logger := provider.Logger(opts.LoggerName)

	return &OLogger{
		Logger: logger,
		Ctx:    ctx,
	}, nil
}
