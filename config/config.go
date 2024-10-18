package config

import "flag"

const DEFAULT_OTLP_LOGGER_NAME = "pve2otelcol"
const DEFAULT_OTLP_GRPC_URL = "http://localhost:4317"
const DEFAULT_OTLP_GRPC_COMPRESSION = "gzip"
const DEFAULT_OTLP_GRPC_RECONNECTION_PERIOD = 10
const DEFAULT_REFRESH_INTERVAL = 10
const DEFAULT_CMD_RETRY_TIMES = 5
const DEFAULT_CMD_RETRY_DELAY = 5

type Config struct {
	OtlpLoggerName             string
	OtlpgRPCURL                string
	OtlpgRPCCompression        string
	OtlpgRPCReconnectionPeriod int
	RefreshInterval            int
	CmdRetryTimes              int
	CmdRetryDelay              int
}

func ParseArgs() *Config {
	c := Config{}
	flag.StringVar(&c.OtlpLoggerName, "otlp-logger-name", DEFAULT_OTLP_LOGGER_NAME, "OpenTelemetry logger name")
	flag.StringVar(&c.OtlpgRPCURL, "otlp-grpc-url", DEFAULT_OTLP_GRPC_URL, "OpenTelemetry gRPC URL")
	flag.StringVar(&c.OtlpgRPCCompression, "otlp-grpc-compression", DEFAULT_OTLP_GRPC_COMPRESSION, "OpenTelemetry gRPC compression algorithm (\"gzip\" or \"none\")")
	flag.IntVar(&c.OtlpgRPCReconnectionPeriod, "otlp-grpc-reconnection-period", DEFAULT_OTLP_GRPC_RECONNECTION_PERIOD, "OpenTelemetry gRPC minimum amount of time between connection attempts to the target endpoint in seconds")
	flag.IntVar(&c.RefreshInterval, "refresh-interval", DEFAULT_REFRESH_INTERVAL, "refresh interval in seconds")
	flag.IntVar(&c.CmdRetryTimes, "cmd-retry-times", DEFAULT_CMD_RETRY_TIMES, "number of times a process is restarted before giving up")
	flag.IntVar(&c.CmdRetryDelay, "cmd-retry-delay", DEFAULT_CMD_RETRY_DELAY, "seconds to wait before a process is restarted on failure")

	flag.Parse()
	return &c
}
