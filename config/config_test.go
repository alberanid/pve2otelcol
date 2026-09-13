package config

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
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

func TestParseArgsUsesDefaults(t *testing.T) {
	cfg, err := ParseArgs(nil)
	if err != nil {
		t.Fatalf("ParseArgs() error = %v, want nil", err)
	}
	if cfg.OtlpExporter != DEFAULT_OTLP_EXPORTER {
		t.Errorf("OTLP exporter = %q, want %q", cfg.OtlpExporter, DEFAULT_OTLP_EXPORTER)
	}
	if cfg.OtlpgRPCURL != DEFAULT_OTLP_GRPC_URL {
		t.Errorf("gRPC URL = %q, want %q", cfg.OtlpgRPCURL, DEFAULT_OTLP_GRPC_URL)
	}
	if cfg.OtlpTimeout != DEFAULT_OTLP_TIMEOUT {
		t.Errorf("OTLP timeout = %d, want %d", cfg.OtlpTimeout, DEFAULT_OTLP_TIMEOUT)
	}
	if cfg.RefreshInterval != DEFAULT_REFRESH_INTERVAL {
		t.Errorf("refresh interval = %d, want %d", cfg.RefreshInterval, DEFAULT_REFRESH_INTERVAL)
	}
	if cfg.DiscoveryTimeout != DEFAULT_DISCOVERY_TIMEOUT {
		t.Errorf("discovery timeout = %d, want %d", cfg.DiscoveryTimeout, DEFAULT_DISCOVERY_TIMEOUT)
	}
	if cfg.CapabilityProbeTimeout != DEFAULT_CAPABILITY_PROBE_TIMEOUT {
		t.Errorf("capability probe timeout = %d, want %d", cfg.CapabilityProbeTimeout, DEFAULT_CAPABILITY_PROBE_TIMEOUT)
	}
	if cfg.MetricsListenAddress != DEFAULT_METRICS_LISTEN_ADDRESS {
		t.Errorf("metrics listen address = %q, want %q", cfg.MetricsListenAddress, DEFAULT_METRICS_LISTEN_ADDRESS)
	}
	if cfg.ConfigFile != DEFAULT_CONFIG_FILE {
		t.Errorf("config file = %q, want %q", cfg.ConfigFile, DEFAULT_CONFIG_FILE)
	}
}

func TestParseArgsUsesIndependentFlagSets(t *testing.T) {
	first, err := ParseArgs([]string{"--verbose", "--monitor-include", "101, 102"})
	if err != nil {
		t.Fatalf("first ParseArgs() error = %v, want nil", err)
	}
	second, err := ParseArgs(nil)
	if err != nil {
		t.Fatalf("second ParseArgs() error = %v, want nil", err)
	}
	if !first.Verbose || len(first.MonitorInclude) != 2 {
		t.Fatalf("first configuration = %#v, want verbose with two included IDs", first)
	}
	if second.Verbose || len(second.MonitorInclude) != 0 {
		t.Fatalf("second configuration leaked first parse state: %#v", second)
	}
}

func TestParseArgsLoadsConfigFileAndCLIOverrides(t *testing.T) {
	path := writeConfig(t, `
[global]
otlp-logger-name = file-logger
otlp-grpc-url = https://file.example:4317
otlp-tls-ca-file = /file/ca.pem
otlp-compression = none
otlp-initial-interval = 3
otlp-max-interval = 12
otlp-max-elapsed-time = 40
otlp-timeout = 9000
otlp-grpc-reconnection-period = 11
otlp-batch-buffer-size = 2
otlp-batch-export-interval = 4
otlp-batch-max-batch-size = 256
refresh-interval = 30
cmd-retry-times = 8
cmd-retry-delay = 6
discovery-timeout = 20
capability-probe-timeout = 7
metrics-listen-address = 127.0.0.1:9999
cursor-dir = /tmp/cursors
skip-lxcs = true
skip-pve = true
dry-run = true
verbose = true

[vms]
include = 101, 102
exclude = 103
`)

	cfg, err := ParseArgs([]string{
		"-config", path,
		"-otlp-logger-name", "cli-logger",
		"-refresh-interval", "15",
		"-skip-lxcs=false",
		"-monitor-include", "201,202",
		"-monitor-exclude", "",
	})
	if err != nil {
		t.Fatalf("ParseArgs() error = %v, want nil", err)
	}
	if cfg.ConfigFile != path || cfg.OtlpLoggerName != "cli-logger" || cfg.RefreshInterval != 15 || cfg.SkipLXCs {
		t.Fatalf("CLI values did not override file values: %#v", cfg)
	}
	if cfg.OtlpCompression != "none" || cfg.OtlpTimeout != 9000 || !cfg.SkipPVE || !cfg.DryRun || !cfg.Verbose {
		t.Fatalf("file values were not loaded: %#v", cfg)
	}
	if !reflect.DeepEqual(cfg.MonitorInclude, []int{201, 202}) || cfg.MonitorExclude != nil {
		t.Fatalf("VM filters = include %v, exclude %v", cfg.MonitorInclude, cfg.MonitorExclude)
	}
}

func TestParseArgsLoadsDefaultConfigWhenPresent(t *testing.T) {
	path := writeConfig(t, "[global]\nverbose = true\n")
	cfg, err := parseArgs(nil, path)
	if err != nil {
		t.Fatalf("parseArgs() error = %v, want nil", err)
	}
	if cfg.ConfigFile != path || !cfg.Verbose {
		t.Fatalf("configuration = %#v, want loaded default file", cfg)
	}
}

func TestParseArgsConfigFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.conf")
	if _, err := ParseArgs([]string{"-config", missing}); err == nil || !strings.Contains(err.Error(), "open config file") {
		t.Fatalf("ParseArgs() error = %v, want missing config file error", err)
	}
	if _, err := parseArgs(nil, missing); err != nil {
		t.Fatalf("parseArgs() error = %v, want optional default file to be ignored", err)
	}

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"outside section", "verbose = true\n", "outside a section"},
		{"unknown section", "[vm]\ninclude = 101\n", "unknown section"},
		{"unknown setting", "[global]\nunknown = true\n", "unknown setting"},
		{"invalid value", "[global]\notlp-timeout = soon\n", "invalid value"},
		{"duplicate setting", "[vms]\ninclude = 101\ninclude = 102\n", "duplicate setting"},
		{"overlapping filters", "[vms]\ninclude = 101\nexclude = 101\n", "present in both"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseArgs([]string{"-config", writeConfig(t, tt.content)})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseArgs() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestSampleConfigCoversConfigurableOptions(t *testing.T) {
	path := filepath.Join("..", "goodies", "pve2otelcol.conf")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseArgs([]string{"-config", path}); err != nil {
		t.Fatalf("ParseArgs(sample) error = %v, want nil", err)
	}

	cfg := Config{}
	var include string
	var exclude string
	flags := newFlagSet(&cfg, &include, &exclude)
	flags.VisitAll(func(f *flag.Flag) {
		if f.Name == "config" || f.Name == "version" || strings.HasPrefix(f.Name, "monitor-") {
			return
		}
		if !strings.Contains(string(content), "\n"+f.Name+" =") {
			t.Errorf("sample config does not contain %q", f.Name)
		}
	})
	for _, setting := range []string{"\ninclude =", "\nexclude ="} {
		if !strings.Contains(string(content), setting) {
			t.Errorf("sample config does not contain %q", strings.TrimSpace(setting))
		}
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pve2otelcol.conf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseArgsReturnsErrors(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErrMsg string
	}{
		{
			name:       "unknown option",
			args:       []string{"--not-an-option"},
			wantErrMsg: "flag provided but not defined",
		},
		{
			name:       "positional argument",
			args:       []string{"unexpected"},
			wantErrMsg: "unexpected positional arguments",
		},
		{
			name:       "malformed include list",
			args:       []string{"--monitor-include", "101,broken"},
			wantErrMsg: "parse monitor-include",
		},
		{
			name:       "overlapping lists",
			args:       []string{"--monitor-include", "101,102", "--monitor-exclude", "102"},
			wantErrMsg: "ID 102 is present in both",
		},
		{
			name:       "invalid selected endpoint",
			args:       []string{"--otlp-grpc-url", "collector.example:4317"},
			wantErrMsg: "otlp-grpc-url must be a valid URL",
		},
		{
			name:       "shared compression error uses shared option name",
			args:       []string{"--otlp-compression", "brotli"},
			wantErrMsg: "otlp-compression must be",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseArgs(tt.args)
			if err == nil {
				t.Fatal("ParseArgs() error = nil, want non-nil")
			}
			if !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Fatalf("ParseArgs() error = %q, want it to contain %q", err, tt.wantErrMsg)
			}
			if strings.Contains(err.Error(), "otlp-grpc-compression") {
				t.Fatalf("ParseArgs() returned stale gRPC-only option name: %q", err)
			}
		})
	}
}

func TestParseArgsHandlesHelpAndVersionWithoutExiting(t *testing.T) {
	_, err := ParseArgs([]string{"--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("ParseArgs(--help) error = %v, want flag.ErrHelp", err)
	}

	cfg, err := ParseArgs([]string{"--version", "--otlp-timeout", "0"})
	if err != nil {
		t.Fatalf("ParseArgs(--version) error = %v, want nil", err)
	}
	if !cfg.Version {
		t.Fatal("ParseArgs(--version) Version = false, want true")
	}
}

func TestPrintUsage(t *testing.T) {
	var output bytes.Buffer
	PrintUsage(&output)
	for _, want := range []string{"Usage: pve2otelcol [options]", "-config", "-otlp-exporter", "-otlp-tls-ca-file", "-refresh-interval", "-metrics-listen-address"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("usage does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestValidateSelectedEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Config)
		wantErrMsg string
	}{
		{
			name: "valid gRPC HTTP URL",
			mutate: func(c *Config) {
				c.OtlpgRPCURL = "http://collector.example:4317"
			},
		},
		{
			name: "valid HTTP exporter HTTPS URL with path",
			mutate: func(c *Config) {
				c.OtlpExporter = "http"
				c.OtlpHTTPURL = "https://collector.example:4318/custom/logs"
			},
		},
		{
			name: "URL scheme is case insensitive",
			mutate: func(c *Config) {
				c.OtlpgRPCURL = "HTTPS://collector.example:4317"
			},
		},
		{
			name: "unselected endpoint is ignored",
			mutate: func(c *Config) {
				c.OtlpHTTPURL = "not a URL"
			},
		},
		{
			name: "missing scheme",
			mutate: func(c *Config) {
				c.OtlpgRPCURL = "collector.example:4317"
			},
			wantErrMsg: "otlp-grpc-url",
		},
		{
			name: "missing host",
			mutate: func(c *Config) {
				c.OtlpgRPCURL = "http:///v1/logs"
			},
			wantErrMsg: "otlp-grpc-url",
		},
		{
			name: "unsupported scheme",
			mutate: func(c *Config) {
				c.OtlpgRPCURL = "ftp://collector.example:4317"
			},
			wantErrMsg: "http or https",
		},
		{
			name: "malformed URL",
			mutate: func(c *Config) {
				c.OtlpgRPCURL = "http://%zz"
			},
			wantErrMsg: "otlp-grpc-url",
		},
		{
			name: "selected HTTP endpoint names HTTP flag",
			mutate: func(c *Config) {
				c.OtlpExporter = "http"
				c.OtlpHTTPURL = ""
			},
			wantErrMsg: "otlp-http-url",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErrMsg == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Fatalf("Validate() error = %v, want it to contain %q", err, tt.wantErrMsg)
			}
		})
	}
}

func TestValidateNumericValuesAndRelationships(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Config)
		wantErrMsg string
	}{
		{"negative initial retry interval", func(c *Config) { c.OtlpInitialInterval = -1 }, "otlp-initial-interval"},
		{"negative maximum retry interval", func(c *Config) { c.OtlpMaxInterval = -1 }, "otlp-max-interval"},
		{"initial exceeds maximum retry interval", func(c *Config) { c.OtlpInitialInterval = c.OtlpMaxInterval + 1 }, "must not exceed"},
		{"negative maximum elapsed time", func(c *Config) { c.OtlpMaxElapsedTime = -1 }, "otlp-max-elapsed-time"},
		{"zero timeout", func(c *Config) { c.OtlpTimeout = 0 }, "otlp-timeout"},
		{"negative timeout", func(c *Config) { c.OtlpTimeout = -1 }, "otlp-timeout"},
		{"negative reconnection period", func(c *Config) { c.OtlpgRPCReconnectionPeriod = -1 }, "otlp-grpc-reconnection-period"},
		{"zero batch buffer", func(c *Config) { c.OtlpBatchBufferSize = 0 }, "otlp-batch-buffer-size"},
		{"zero batch export interval", func(c *Config) { c.OtlpBatchExportInterval = 0 }, "otlp-batch-export-interval"},
		{"zero maximum batch size", func(c *Config) { c.OtlpBatchMaxBatchSize = 0 }, "otlp-batch-max-batch-size"},
		{"negative refresh interval", func(c *Config) { c.RefreshInterval = -1 }, "refresh-interval"},
		{"negative command retry count", func(c *Config) { c.CmdRetryTimes = -1 }, "cmd-retry-times"},
		{"negative command retry delay", func(c *Config) { c.CmdRetryDelay = -1 }, "cmd-retry-delay"},
		{"zero discovery timeout", func(c *Config) { c.DiscoveryTimeout = 0 }, "discovery-timeout"},
		{"negative discovery timeout", func(c *Config) { c.DiscoveryTimeout = -1 }, "discovery-timeout"},
		{"zero capability probe timeout", func(c *Config) { c.CapabilityProbeTimeout = 0 }, "capability-probe-timeout"},
		{"negative capability probe timeout", func(c *Config) { c.CapabilityProbeTimeout = -1 }, "capability-probe-timeout"},
		{"metrics address without port", func(c *Config) { c.MetricsListenAddress = "localhost" }, "metrics-listen-address"},
		{"metrics address with zero port", func(c *Config) { c.MetricsListenAddress = "localhost:0" }, "metrics-listen-address port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Fatalf("Validate() error = %v, want it to contain %q", err, tt.wantErrMsg)
			}
		})
	}
}

func TestValidateAllowsDocumentedZeroValues(t *testing.T) {
	cfg := validConfig()
	cfg.OtlpInitialInterval = 0
	cfg.OtlpMaxInterval = 0
	cfg.OtlpMaxElapsedTime = 0
	cfg.OtlpgRPCReconnectionPeriod = 0
	cfg.RefreshInterval = 0
	cfg.CmdRetryTimes = 0
	cfg.CmdRetryDelay = 0
	cfg.MetricsListenAddress = ""

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for documented zero values", err)
	}
}

func validConfig() Config {
	return Config{
		OtlpExporter:               DEFAULT_OTLP_EXPORTER,
		OtlpgRPCURL:                DEFAULT_OTLP_GRPC_URL,
		OtlpHTTPURL:                DEFAULT_OTLP_HTTP_URL,
		OtlpCompression:            DEFAULT_OTLP_COMPRESSION,
		OtlpInitialInterval:        DEFAULT_OTLP_INITIAL_INTERVAL,
		OtlpMaxInterval:            DEFAULT_OTLP_MAX_INTERVAL,
		OtlpMaxElapsedTime:         DEFAULT_OTLP_MAX_ELAPSED_TIME,
		OtlpTimeout:                DEFAULT_OTLP_TIMEOUT,
		OtlpBatchBufferSize:        DEFAULT_OTLP_BATCH_BUFFER_SIZE,
		OtlpBatchExportInterval:    DEFAULT_OTLP_BATCH_EXPORT_INTERVAL,
		OtlpBatchMaxBatchSize:      DEFAULT_OTLP_BATCH_MAX_BATCH_SIZE,
		OtlpgRPCReconnectionPeriod: DEFAULT_OTLP_GRPC_RECONNECTION_PERIOD,
		RefreshInterval:            DEFAULT_REFRESH_INTERVAL,
		CmdRetryTimes:              DEFAULT_CMD_RETRY_TIMES,
		CmdRetryDelay:              DEFAULT_CMD_RETRY_DELAY,
		DiscoveryTimeout:           DEFAULT_DISCOVERY_TIMEOUT,
		CapabilityProbeTimeout:     DEFAULT_CAPABILITY_PROBE_TIMEOUT,
		MetricsListenAddress:       DEFAULT_METRICS_LISTEN_ADDRESS,
	}
}
