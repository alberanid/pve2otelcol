package config

import (
	"strings"
	"testing"
)

func TestValidateTLS(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		wantErrMsg string
	}{
		{
			name: "plaintext endpoint without TLS files",
			cfg: Config{
				OtlpExporter: "grpc",
				OtlpgRPCURL:  "http://collector.example:4317",
			},
		},
		{
			name: "additional CA with secure gRPC endpoint",
			cfg: Config{
				OtlpExporter:  "grpc",
				OtlpgRPCURL:   "https://collector.example:4317",
				OtlpTLSCAFile: "ca.pem",
			},
		},
		{
			name: "client identity with secure HTTP endpoint",
			cfg: Config{
				OtlpExporter:    "http",
				OtlpHTTPURL:     "https://collector.example:4318",
				OtlpTLSCertFile: "client.pem",
				OtlpTLSKeyFile:  "client-key.pem",
			},
		},
		{
			name: "client certificate without key",
			cfg: Config{
				OtlpExporter:    "grpc",
				OtlpgRPCURL:     "https://collector.example:4317",
				OtlpTLSCertFile: "client.pem",
			},
			wantErrMsg: "must both be specified",
		},
		{
			name: "client key without certificate",
			cfg: Config{
				OtlpExporter:   "grpc",
				OtlpgRPCURL:    "https://collector.example:4317",
				OtlpTLSKeyFile: "client-key.pem",
			},
			wantErrMsg: "must both be specified",
		},
		{
			name: "additional CA with plaintext gRPC endpoint",
			cfg: Config{
				OtlpExporter:  "grpc",
				OtlpgRPCURL:   "http://collector.example:4317",
				OtlpTLSCAFile: "ca.pem",
			},
			wantErrMsg: "use https",
		},
		{
			name: "client identity with plaintext HTTP endpoint",
			cfg: Config{
				OtlpExporter:    "http",
				OtlpHTTPURL:     "http://collector.example:4318",
				OtlpTLSCertFile: "client.pem",
				OtlpTLSKeyFile:  "client-key.pem",
			},
			wantErrMsg: "use https",
		},
		{
			name: "TLS files with malformed endpoint",
			cfg: Config{
				OtlpExporter:  "grpc",
				OtlpgRPCURL:   "://collector.example",
				OtlpTLSCAFile: "ca.pem",
			},
			wantErrMsg: "use https",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateTLS()
			if tt.wantErrMsg == "" {
				if err != nil {
					t.Fatalf("validateTLS() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateTLS() error = nil, want non-nil")
			}
			if !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Fatalf("validateTLS() error = %q, want it to contain %q", err, tt.wantErrMsg)
			}
		})
	}
}
