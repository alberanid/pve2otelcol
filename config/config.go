package config

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/alberanid/pve2otelcol/version"
)

const DEFAULT_OTLP_LOGGER_NAME = "pve2otelcol"
const DEFAULT_OTLP_EXPORTER = "grpc"
const DEFAULT_OTLP_GRPC_URL = "http://localhost:4317"
const DEFAULT_OTLP_HTTP_URL = "https://localhost:4318"
const DEFAULT_OTLP_COMPRESSION = "gzip"
const DEFAULT_OTLP_GRPC_RECONNECTION_PERIOD = 10
const DEFAULT_OTLP_INITIAL_INTERVAL = 2
const DEFAULT_OTLP_MAX_INTERVAL = 10
const DEFAULT_OTLP_MAX_ELAPSED_TIME = 30
const DEFAULT_OTLP_TIMEOUT = 10000
const DEFAULT_OTLP_BATCH_BUFFER_SIZE = 1
const DEFAULT_OTLP_BATCH_EXPORT_INTERVAL = 1
const DEFAULT_OTLP_BATCH_MAX_BATCH_SIZE = 512
const DEFAULT_REFRESH_INTERVAL = 10
const DEFAULT_CMD_RETRY_TIMES = 5
const DEFAULT_CMD_RETRY_DELAY = 5

// store command line configuration.
type Config struct {
	OtlpLoggerName             string
	OtlpExporter               string
	OtlpgRPCURL                string
	OtlpHTTPURL                string
	OtlpTLSCertFile            string
	OtlpTLSKeyFile             string
	OtlpCompression            string
	OtlpInitialInterval        int
	OtlpMaxInterval            int
	OtlpMaxElapsedTime         int
	OtlpTimeout                int
	OtlpBatchBufferSize        int
	OtlpBatchExportInterval    int
	OtlpBatchMaxBatchSize      int
	OtlpgRPCReconnectionPeriod int

	RefreshInterval int
	CmdRetryTimes   int
	CmdRetryDelay   int
	SkipLXCs        bool
	SkipPVE         bool
	//SkipKVMs     	bool
	MonitorInclude []int
	MonitorExclude []int

	DryRun  bool
	Verbose bool
}

// Split and trim comma-separated values
func splitAndTrim(s string) []int {
	ids := []int{}
	parts := strings.Split(s, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		id, err := strconv.Atoi(part)
		if err != nil {
			slog.Error(fmt.Sprintf("include and exclude list items must be integers; wrong value: '%s'", part))
			flag.PrintDefaults()
			os.Exit(1)
		}
		ids = append(ids, id)
	}
	return ids
}

// parse command line arguments.
func ParseArgs() *Config {
	c := Config{}
	flag.StringVar(&c.OtlpLoggerName, "otlp-logger-name", DEFAULT_OTLP_LOGGER_NAME, "OpenTelemetry logger name")

	flag.StringVar(&c.OtlpgRPCURL, "otlp-exporter", DEFAULT_OTLP_EXPORTER, "OpenTelemetry exporter (\"grpc\" or \"http\")")
	flag.StringVar(&c.OtlpgRPCURL, "otlp-grpc-url", DEFAULT_OTLP_GRPC_URL, "OpenTelemetry gRPC URL")
	flag.StringVar(&c.OtlpHTTPURL, "otlp-http-url", DEFAULT_OTLP_HTTP_URL, "OpenTelemetry HTTP URL")

	flag.StringVar(&c.OtlpTLSCertFile, "otlp-tls-cert-file", "", "Path to the TLS certificate file")
	flag.StringVar(&c.OtlpTLSKeyFile, "otlp-tls-key-file", "", "Path to the TLS key file")
	flag.StringVar(&c.OtlpCompression, "otlp-compression", DEFAULT_OTLP_COMPRESSION,
		"OpenTelemetry compression algorithm (\"gzip\" or \"none\")")
	flag.IntVar(&c.OtlpInitialInterval, "otlp-initial-interval",
		DEFAULT_OTLP_INITIAL_INTERVAL, "OpenTelemetry time to wait after the first failure before retrying in seconds")
	flag.IntVar(&c.OtlpMaxInterval, "otlp-max-interval",
		DEFAULT_OTLP_MAX_INTERVAL, "OpenTelemetry upper bound on backoff interval in seconds")
	flag.IntVar(&c.OtlpMaxElapsedTime, "otlp-max-elapsed-time",
		DEFAULT_OTLP_MAX_ELAPSED_TIME, "OpenTelemetry maximum amount of time (including retries) spent trying to send a request/batch in seconds")
	flag.IntVar(&c.OtlpTimeout, "otlp-timeout",
		DEFAULT_OTLP_TIMEOUT, "OpenTelemetry timeout in milliseconds")

	flag.IntVar(&c.OtlpgRPCReconnectionPeriod, "otlp-grpc-reconnection-period",
		DEFAULT_OTLP_GRPC_RECONNECTION_PERIOD, "OpenTelemetry minimum amount of time between connection attempts to the target endpoint in seconds")

	flag.IntVar(&c.OtlpBatchBufferSize, "otlp-batch-buffer-size",
		DEFAULT_OTLP_BATCH_BUFFER_SIZE, "OpenTelemetry batch buffer size that is kept in memory")
	flag.IntVar(&c.OtlpBatchExportInterval, "otlp-batch-export-interval",
		DEFAULT_OTLP_BATCH_EXPORT_INTERVAL, "OpenTelemetry maximum duration between batched exports in seconds")
	flag.IntVar(&c.OtlpBatchMaxBatchSize, "otlp-batch-max-batch-size",
		DEFAULT_OTLP_BATCH_MAX_BATCH_SIZE, "OpenTelemetry maximum batch size of every export")

	flag.IntVar(&c.RefreshInterval, "refresh-interval", DEFAULT_REFRESH_INTERVAL, "refresh interval in seconds")
	flag.IntVar(&c.CmdRetryTimes, "cmd-retry-times", DEFAULT_CMD_RETRY_TIMES, "number of times a process is restarted before giving up")
	flag.IntVar(&c.CmdRetryDelay, "cmd-retry-delay", DEFAULT_CMD_RETRY_DELAY, "seconds to wait before a process is restarted on failure")
	flag.BoolVar(&c.SkipLXCs, "skip-lxcs", false, "do not monitor LXCs virtuals")
	flag.BoolVar(&c.SkipPVE, "skip-pve", false, "do not monitor this PVE node")
	// it will be reintroduced if we'll find a way to get the stdout stream from a qm exec command.
	//flag.BoolVar(&c.SkipKVMs, "skip-vms", false, "do not consider Qemu/KVM virtuals")
	var monitorInclude string
	var monitorExclude string
	flag.StringVar(&monitorInclude, "monitor-include", "", "Comma-separated list of IDs to include in monitoring")
	flag.StringVar(&monitorExclude, "monitor-exclude", "", "Comma-separated list of IDs to exclude from monitoring")

	flag.BoolVar(&c.DryRun, "dry-run", false, "do not execute any command")
	flag.BoolVar(&c.Verbose, "verbose", false, "be more verbose")
	getVer := flag.Bool("version", false, "print version and quit")

	flag.Parse()

	if *getVer {
		fmt.Printf("version %s\n", version.VERSION)
		os.Exit(0)
	}

	if c.OtlpExporter != "grpc" && c.OtlpExporter != "http" {
		slog.Error("otlp-exporter must be \"grpc\" or \"http\"")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if (c.OtlpTLSCertFile != "" || c.OtlpTLSKeyFile != "") &&
		!(c.OtlpTLSCertFile != "" && c.OtlpTLSKeyFile != "") {
		slog.Error("otlp-grpc-tls-cert-file and otlp-grpc-tls-key-file must both be specified")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if c.OtlpCompression != "none" && c.OtlpCompression != "gzip" {
		slog.Error("otlp-grpc-compression must be \"none\" or \"gzip\"")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if c.OtlpgRPCReconnectionPeriod < 0 {
		slog.Error("otlp-grpc-reconnection-period must be equal or greater than zero")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if c.OtlpBatchBufferSize < 1 {
		slog.Error("otlp-batch-buffer-size must be greater than zero")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if c.OtlpBatchExportInterval < 1 {
		slog.Error("otlp-batch-export-interval must be greater than zero")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if c.OtlpBatchMaxBatchSize < 1 {
		slog.Error("otlp-batch-max-batch-size must be greater than zero")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if c.RefreshInterval < 0 {
		slog.Error("refresh-interval must be equal or greater than zero")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if c.CmdRetryTimes < 0 {
		slog.Error("cmd-retry-times must be equal or greater than zero")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if c.CmdRetryDelay < 0 {
		slog.Error("cmd-retry-delay must be equal or greater than zero")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if c.Verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	if monitorInclude != "" {
		c.MonitorInclude = splitAndTrim(monitorInclude)
	}
	if monitorExclude != "" {
		c.MonitorExclude = splitAndTrim(monitorExclude)
	}
	for _, id := range c.MonitorInclude {
		if slices.Contains(c.MonitorExclude, id) {
			slog.Error(fmt.Sprintf("error: ID %d is present in both include and exclude lists", id))
			flag.PrintDefaults()
			os.Exit(1)
		}
	}

	return &c
}
