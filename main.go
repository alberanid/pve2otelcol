package main

import (
	"context"
	"log"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	otellog "go.opentelemetry.io/otel/log"
	otelsdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func main() {
	ctx := context.Background()
	rpcOptions := []otlploggrpc.Option{
		otlploggrpc.WithEndpointURL("http://alloy.lan:4317"),
		otlploggrpc.WithCompressor("gzip"),
		otlploggrpc.WithReconnectionPeriod(time.Duration(time.Duration(10) * time.Second)),
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: time.Duration(2) * time.Second,
			MaxInterval:     time.Duration(10) * time.Second,
			MaxElapsedTime:  time.Duration(30) * time.Second,
		}),
	}
	rpc, err := otlploggrpc.New(ctx, rpcOptions...)
	if err != nil {
		log.Fatal("gne gne gne")
	}
	r := otelsdklog.Record{}
	r.SetBody(otellog.StringValue("test log"))

	records := []otelsdklog.Record{}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("lxc/666"),
		),
	)

	bp := otelsdklog.NewBatchProcessor(rpc)
	provider := otelsdklog.NewLoggerProvider(
		otelsdklog.WithProcessor(bp),
		otelsdklog.WithResource(res),
	)
	tl := provider.Logger("test logger")

	olog := otellog.Record{}
	olog.SetBody(otellog.StringValue("test log 2"))
	tl.Emit(ctx, olog)
	provider.ForceFlush(ctx)

	records = append(records, r)
	err = rpc.Export(ctx, records)
	if err != nil {
		log.Printf("error exporting: %v", err)
	}
}
