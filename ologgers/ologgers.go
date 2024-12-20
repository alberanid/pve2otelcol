package ologgers

/*
Interface to the OpenTelemetry modules.
*/

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/alberanid/pve2otelcol/config"
	"google.golang.org/grpc/credentials"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// map syslog severity levels (priority, in systemd) to OTLP severity.
// We use only main levels, to prevent loki ingestor warnings like "msg="unknown log level while observing stream" level=info2".
// Ideally intermediate levels should be used; see:
// https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/logs/data-model-appendix.md#appendix-b-severitynumber-example-mappings
var prio2severity = map[string]otellog.Severity{
	"0": otellog.SeverityFatal,
	"1": otellog.SeverityError, // ideally SeverityError3
	"2": otellog.SeverityError, // ideally SeverityError2
	"3": otellog.SeverityError,
	"4": otellog.SeverityWarn,
	"5": otellog.SeverityInfo, // ideally, SeverityInfo2
	"6": otellog.SeverityInfo,
	"7": otellog.SeverityDebug,
}

var prio2string = map[string]string{
	"0": "FATAL",
	"1": "ERROR",
	"2": "ERROR",
	"3": "ERROR",
	"4": "WARN",
	"5": "INFO",
	"6": "INFO",
	"7": "DEBUG",
}

// Transform an interface to an object suitable to be logged by OpenTelemetry
func transformBody(i interface{}) otellog.Value {
	// the OpenTelemetry SDK replaces JSON null or unknown values to the "INVALID" string, which is an odd choice;
	// here we stay consistent with this behavior returning a string, but at least it's empty.
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

// convert a string timestamp in microseconds to a time.Time instance
func str2time(s string) (time.Time, error) {
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Now(), nil
	}
	secs := int64(i / 1000000)
	micros := int64(i%1000000) * 1000
	tm := time.Unix(secs, micros)
	return tm, nil
}

// Object used to log to an OpenTelemetry instance
type OLogger struct {
	Logger otellog.Logger
	Ctx    context.Context
}

// Options of an OLogger instance
type OLoggerOptions struct {
	ServiceId   string
	ServiceName string
}

// Create an OLogger instance
func New(cfg *config.Config, opts OLoggerOptions) (*OLogger, error) {
	ctx := context.Background()
	var exporter sdklog.Exporter
	var err error

	if cfg.OtlpgRPCURL != "" {
		rpcOptions := []otlploggrpc.Option{
			otlploggrpc.WithEndpointURL(cfg.OtlpgRPCURL),
			otlploggrpc.WithCompressor(cfg.OtlpCompression),
			otlploggrpc.WithReconnectionPeriod(time.Duration(cfg.OtlpgRPCReconnectionPeriod) * time.Second),
			otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
				Enabled:         true,
				InitialInterval: time.Duration(cfg.OtlpInitialInterval) * time.Second,
				MaxInterval:     time.Duration(cfg.OtlpMaxInterval) * time.Second,
				MaxElapsedTime:  time.Duration(cfg.OtlpMaxElapsedTime) * time.Second,
			}),
		}

		if cfg.OtlpTLSCertFile != "" && cfg.OtlpTLSKeyFile != "" {
			certificate, err := tls.LoadX509KeyPair(cfg.OtlpTLSCertFile, cfg.OtlpTLSKeyFile)
			if err != nil {
				slog.Error(fmt.Sprintf("failed to load TLS certificate and key: %v", err))
				return nil, err
			}

			certPool := x509.NewCertPool()
			ca, err := os.ReadFile(cfg.OtlpTLSCertFile)
			if err != nil {
				slog.Error(fmt.Sprintf("failed to read CA certificate: %v", err))
				return nil, err
			}

			if ok := certPool.AppendCertsFromPEM(ca); !ok {
				slog.Error("failed to append CA certificate to cert pool")
				return nil, fmt.Errorf("failed to append CA certificate to cert pool")
			}

			creds := credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{certificate},
				RootCAs:      certPool,
			})

			rpcOptions = append(rpcOptions, otlploggrpc.WithTLSCredentials(creds))
		}

		exporter, err = otlploggrpc.New(ctx, rpcOptions...)
		if err != nil {
			slog.Error(fmt.Sprintf("failure creating gRPC logger with options %v; error: %v", opts, err))
			return nil, err
		}
	} else if cfg.OtlpHTTPURL != "" {
		httpOptions := []otlploghttp.Option{
			otlploghttp.WithEndpointURL(cfg.OtlpHTTPURL),
			otlploghttp.WithRetry(otlploghttp.RetryConfig{
				Enabled:         true,
				InitialInterval: time.Duration(cfg.OtlpInitialInterval) * time.Second,
				MaxInterval:     time.Duration(cfg.OtlpMaxInterval) * time.Second,
				MaxElapsedTime:  time.Duration(cfg.OtlpMaxElapsedTime) * time.Second,
			}),
		}
		if cfg.OtlpCompression == "gzip" {
			httpOptions = append(httpOptions, otlploghttp.WithCompression(otlploghttp.GzipCompression))
		}

		if cfg.OtlpTLSCertFile != "" && cfg.OtlpTLSKeyFile != "" {
			certificate, err := tls.LoadX509KeyPair(cfg.OtlpTLSCertFile, cfg.OtlpTLSKeyFile)
			if err != nil {
				slog.Error(fmt.Sprintf("failed to load TLS certificate and key: %v", err))
				return nil, err
			}

			certPool := x509.NewCertPool()
			ca, err := os.ReadFile(cfg.OtlpTLSCertFile)
			if err != nil {
				slog.Error(fmt.Sprintf("failed to read CA certificate: %v", err))
				return nil, err
			}

			if ok := certPool.AppendCertsFromPEM(ca); !ok {
				slog.Error("failed to append CA certificate to cert pool")
				return nil, fmt.Errorf("failed to append CA certificate to cert pool")
			}

			httpOptions = append(httpOptions, otlploghttp.WithTLSClientConfig(&tls.Config{
				Certificates: []tls.Certificate{certificate},
				RootCAs:      certPool,
			}))
		}

		exporter, err = otlploghttp.New(ctx, httpOptions...)
		if err != nil {
			slog.Error(fmt.Sprintf("failure creating HTTP logger with options %v; error: %v", opts, err))
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("no valid OTLP endpoint provided")
	}

	providerResources, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceInstanceID(opts.ServiceId),
		),
	)
	if err != nil {
		slog.Error(fmt.Sprintf("failure setting service instance id of logger; error: %v", err))
		return nil, err
	}
	providerResources, err = resource.Merge(
		providerResources,
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(opts.ServiceName),
		),
	)
	if err != nil {
		slog.Error(fmt.Sprintf("failure setting service name of logger; error: %v", err))
		return nil, err
	}

	processor := sdklog.NewBatchProcessor(exporter,
		sdklog.WithExportBufferSize(cfg.OtlpBatchBufferSize),
		sdklog.WithExportInterval(time.Duration(cfg.OtlpBatchExportInterval)*time.Second),
		sdklog.WithExportMaxBatchSize(cfg.OtlpBatchMaxBatchSize))
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(processor),
		sdklog.WithResource(providerResources),
	)
	logger := provider.Logger(cfg.OtlpLoggerName)

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
	for _, kv := range body.AsMap() {
		if kv.Key == "_SOURCE_REALTIME_TIMESTAMP" {
			tm, err := str2time(kv.Value.AsString())
			if err != nil {
				record.SetTimestamp(tm)
			}
		} else if kv.Key == "__REALTIME_TIMESTAMP" {
			tm, err := str2time(kv.Value.AsString())
			if err != nil {
				record.SetObservedTimestamp(tm)
			}
		} else if kv.Key == "PRIORITY" {
			if severity, ok := prio2severity[kv.Value.AsString()]; ok {
				record.SetSeverity(severity)
			}
			if severityTxt, ok := prio2string[kv.Value.AsString()]; ok {
				record.SetSeverityText(severityTxt)
			}
		} else if kv.Key == "_PID" {
			i, err := strconv.Atoi(kv.Value.AsString())
			if err == nil {
				record.AddAttributes(otellog.KeyValue{
					Key:   "pid",
					Value: otellog.IntValue(i),
				})
			}
		} else if kv.Key == "_COMM" {
			record.AddAttributes(otellog.KeyValue{
				Key:   "command",
				Value: otellog.StringValue(kv.Value.AsString()),
			})
		}
	}
	o.LogRecord(record)
}
