package config

import "flag"

const DEFAULT_OTLP_GRPC_URL = "http://localhost:4317"
const DEFAULT_REFRESH_INTERVAL = 10

type Config struct {
	OtlpgRPCURL     string
	RefreshInterval int
}

func ParseArgs() *Config {
	c := Config{}
	flag.StringVar(&c.OtlpgRPCURL, "otlp-grpc-url", DEFAULT_OTLP_GRPC_URL, "OpenTelemetry gRPC URL")
	flag.IntVar(&c.RefreshInterval, "refresh-interval", DEFAULT_REFRESH_INTERVAL, "refresh interval in seconds")
	flag.Parse()
	return &c
}
