package ologgers

/*
Interface to the OpenTelemetry modules.
*/

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Transform an interface to an object suitable to be logged by OpenTelemetry
func transformBody(i interface{}) otellog.Value {
	_emptyValue := otellog.StringValue("")
	switch obj := i.(type) {
	case string:
		return otellog.StringValue(obj)
	case []byte:
		return otellog.BytesValue(obj)
	case int:
		return otellog.IntValue(obj)
	case float32:
		return otellog.Float64Value(float64(obj))
	case float64:
		return otellog.Float64Value(obj)
	case bool:
		return otellog.BoolValue(obj)
	case map[string]interface{}:
		ret := []otellog.KeyValue{}
		for key, value := range obj {
			oval := transformBody(value)
			if oval.Empty() {
				oval = _emptyValue
			}
			ret = append(ret, otellog.KeyValue{
				Key:   key,
				Value: oval,
			})
		}
		return otellog.MapValue(ret...)
	case []interface{}:
		ret := []otellog.Value{}
		for _, i := range obj {
			oval := transformBody(i)
			if oval.Empty() {
				oval = _emptyValue
			}
			ret = append(ret, oval)
		}
		return otellog.SliceValue(ret...)
	case nil:
		return _emptyValue
	default:
		return _emptyValue
	}
}

// Object used to log to an OpenTelemetry instance
type OLogger struct {
	Logger otellog.Logger
	Ctx    context.Context
}

// Options of an OLogger instance
type OLoggerOptions struct {
	Endpoint    string
	ServiceName string
	LoggerName  string
}

// Create an OLogger instance
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

// Emit a Record
func (o *OLogger) LogRecord(r otellog.Record) {
	o.Logger.Emit(o.Ctx, r)
}

// Log any object
func (o *OLogger) Log(i interface{}) {
	body := transformBody(i)
	record := otellog.Record{}
	record.SetBody(body)
	o.LogRecord(record)
}
