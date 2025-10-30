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
	"os/signal"
	"strconv"
	"syscall"
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
	Logger   otellog.Logger
	Ctx      context.Context
	Provider *sdklog.LoggerProvider
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

	withTLS := cfg.OtlpTLSCertFile != "" && cfg.OtlpTLSKeyFile != ""
	tlsConfig := tls.Config{}
	if withTLS {
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

		tlsConfig = tls.Config{
			Certificates: []tls.Certificate{certificate},
			RootCAs:      certPool,
		}
	}

	if cfg.OtlpExporter == "grpc" {
		rpcOptions := []otlploggrpc.Option{
			otlploggrpc.WithEndpointURL(cfg.OtlpgRPCURL),
			otlploggrpc.WithCompressor(cfg.OtlpCompression),
			otlploggrpc.WithReconnectionPeriod(time.Duration(cfg.OtlpgRPCReconnectionPeriod) * time.Second),
			otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
				Enabled:         true,
				InitialInterval: time.Duration(cfg.OtlpInitialInterval) * time.Second,
				MaxInterval:     time.Duration(cfg.OtlpMaxInterval) * time.Second,
				MaxElapsedTime:  time.Duration(cfg.OtlpMaxElapsedTime) * time.Second,
			},
			),
			otlploggrpc.WithTimeout(time.Duration(cfg.OtlpTimeout) * time.Millisecond),
		}

		if withTLS {
			creds := credentials.NewTLS(&tlsConfig)
			rpcOptions = append(rpcOptions, otlploggrpc.WithTLSCredentials(creds))
		}

		exporter, err = otlploggrpc.New(ctx, rpcOptions...)
		if err != nil {
			slog.Error(fmt.Sprintf("failure creating gRPC logger with options %v; error: %v", opts, err))
			return nil, err
		}
	} else if cfg.OtlpExporter == "http" {
		httpOptions := []otlploghttp.Option{
			otlploghttp.WithEndpointURL(cfg.OtlpHTTPURL),
			otlploghttp.WithRetry(otlploghttp.RetryConfig{
				Enabled:         true,
				InitialInterval: time.Duration(cfg.OtlpInitialInterval) * time.Second,
				MaxInterval:     time.Duration(cfg.OtlpMaxInterval) * time.Second,
				MaxElapsedTime:  time.Duration(cfg.OtlpMaxElapsedTime) * time.Second,
			}),
			otlploghttp.WithTimeout(time.Duration(cfg.OtlpTimeout) * time.Millisecond),
		}
		if cfg.OtlpCompression == "gzip" {
			httpOptions = append(httpOptions, otlploghttp.WithCompression(otlploghttp.GzipCompression))
		}

		if withTLS {
			httpOptions = append(httpOptions, otlploghttp.WithTLSClientConfig(&tlsConfig))
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

	ol := &OLogger{
		Logger:   logger,
		Ctx:      ctx,
		Provider: provider,
	}

	// Ensure we flush pending logs on application shutdown signals.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		sig := <-sigCh
		slog.Info("received signal, shutting down otel logger to flush pending logs", "signal", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ol.Provider.Shutdown(shutdownCtx); err != nil {
			slog.Error(fmt.Sprintf("error shutting down otel logger: %v", err))
		}
		// exit to honor the signal and ensure process termination after flushing
		os.Exit(0)
	}()

	return ol, nil
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
		switch kv.Key {
		case "_SOURCE_REALTIME_TIMESTAMP":
			tm, err := str2time(kv.Value.AsString())
			if err == nil {
				record.SetTimestamp(tm)
			}
		case "__REALTIME_TIMESTAMP":
			tm, err := str2time(kv.Value.AsString())
			if err == nil {
				record.SetObservedTimestamp(tm)
			}
		case "PRIORITY":
			if severity, ok := prio2severity[kv.Value.AsString()]; ok {
				record.SetSeverity(severity)
			}
			if severityTxt, ok := prio2string[kv.Value.AsString()]; ok {
				record.SetSeverityText(severityTxt)
			}
		case "_PID":
			pid, err := strconv.Atoi(kv.Value.AsString())
			if err == nil {
				record.AddAttributes(otellog.KeyValue{
					Key:   "pid",
					Value: otellog.IntValue(pid),
				})
			}
		case "_COMM":
			record.AddAttributes(otellog.KeyValue{
				Key:   "command",
				Value: otellog.StringValue(kv.Value.AsString()),
			})
		}
	}
	o.LogRecord(record)
}

// Shutdown flushes pending logs and shuts down the logger provider.
func (o *OLogger) Shutdown(ctx context.Context) error {
	if o.Provider == nil {
		return nil
	}
	return o.Provider.Shutdown(ctx)
}
