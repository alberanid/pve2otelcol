package config

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/alberanid/pve2otelcol/version"
)

const DEFAULT_OTLP_LOGGER_NAME = "pve2otelcol"
const DEFAULT_OTLP_GRPC_URL = "http://localhost:4317"
const DEFAULT_OTLP_GRPC_COMPRESSION = "gzip"
const DEFAULT_OTLP_GRPC_RECONNECTION_PERIOD = 10
const DEFAULT_OTLP_GRPC_INITIAL_INTERVAL = 2
const DEFAULT_OTLP_GRPC_MAX_INTERVAL = 10
const DEFAULT_OTLP_GRPC_MAX_ELAPSED_TIME = 30
const DEFAULT_REFRESH_INTERVAL = 10
const DEFAULT_CMD_RETRY_TIMES = 5
const DEFAULT_CMD_RETRY_DELAY = 5

// store command line configuration.
type Config struct {
	OtlpLoggerName string

	OtlpgRPCURL                string
	OtlpgRPCCompression        string
	OtlpgRPCReconnectionPeriod int
	OtlpgRPCInitialInterval    int
	OtlpgRPCMaxInterval        int
	OtlpgRPCMaxElapsedTime     int

	RefreshInterval int
	CmdRetryTimes   int
	CmdRetryDelay   int
	SkipLXCs        bool
	//SkipKVMs     	bool

	Verbose bool
}

// parse command line arguments.
func ParseArgs() *Config {
	c := Config{}
	flag.StringVar(&c.OtlpLoggerName, "otlp-logger-name", DEFAULT_OTLP_LOGGER_NAME, "OpenTelemetry logger name")

	flag.StringVar(&c.OtlpgRPCURL, "otlp-grpc-url", DEFAULT_OTLP_GRPC_URL, "OpenTelemetry gRPC URL")
	flag.StringVar(&c.OtlpgRPCCompression, "otlp-grpc-compression", DEFAULT_OTLP_GRPC_COMPRESSION,
		"OpenTelemetry gRPC compression algorithm (\"gzip\" or \"none\")")
	flag.IntVar(&c.OtlpgRPCReconnectionPeriod, "otlp-grpc-reconnection-period",
		DEFAULT_OTLP_GRPC_RECONNECTION_PERIOD, "OpenTelemetry gRPC minimum amount of time between connection attempts to the target endpoint in seconds")
	flag.IntVar(&c.OtlpgRPCInitialInterval, "otlp-grpc-initial-interval",
		DEFAULT_OTLP_GRPC_INITIAL_INTERVAL, "OpenTelemetry gRPC time to wait after the first failure before retrying in seconds")
	flag.IntVar(&c.OtlpgRPCMaxInterval, "otlp-grpc-max-interval",
		DEFAULT_OTLP_GRPC_MAX_INTERVAL, "OpenTelemetry gRPC upper bound on backoff interval in seconds")
	flag.IntVar(&c.OtlpgRPCMaxElapsedTime, "otlp-grpc-max-elapsed-time",
		DEFAULT_OTLP_GRPC_MAX_ELAPSED_TIME, "OpenTelemetry gRPC maximum amount of time (including retries) spent trying to send a request/batch in seconds")

	flag.IntVar(&c.RefreshInterval, "refresh-interval", DEFAULT_REFRESH_INTERVAL, "refresh interval in seconds")
	flag.IntVar(&c.CmdRetryTimes, "cmd-retry-times", DEFAULT_CMD_RETRY_TIMES, "number of times a process is restarted before giving up")
	flag.IntVar(&c.CmdRetryDelay, "cmd-retry-delay", DEFAULT_CMD_RETRY_DELAY, "seconds to wait before a process is restarted on failure")
	flag.BoolVar(&c.SkipLXCs, "skip-lxcs", false, "do not consider LXCs virtuals")
	// it will be reintroduced if we'll find a way to get the stdout stream from a qm exec command.
	//flag.BoolVar(&c.SkipKVMs, "skip-vms", false, "do not consider Qemu/KVM virtuals")

	flag.BoolVar(&c.Verbose, "verbose", false, "be more verbose")
	getVer := flag.Bool("version", false, "print version and quit")
	flag.Parse()

	if *getVer {
		fmt.Printf("version %s\n", version.VERSION)
		os.Exit(0)
	}

	if c.OtlpgRPCCompression != "none" && c.OtlpgRPCCompression != "gzip" {
		slog.Error("otlp-grpc-compression must be \"none\" or \"gzip\"")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if c.OtlpgRPCReconnectionPeriod < 0 {
		slog.Error("otlp-grpc-reconnection-period must be equal or greater than zero")
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

	return &c
}
