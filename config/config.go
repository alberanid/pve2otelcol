package config

import "flag"

type Config struct {
	OtlpgRPCURL     string
	RefreshInterval int
}

func ParseArgs() *Config {
	c := Config{}
	flag.StringVar(&c.OtlpgRPCURL, "otlp-grpc-url", "", "")
	flag.IntVar(&c.RefreshInterval, "refresh-interval", 10, "")
	flag.Parse()
	return &c
}
