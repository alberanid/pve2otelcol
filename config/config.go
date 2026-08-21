package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
const DEFAULT_CURSOR_DIR = "/var/lib/pve2otelcol/cursors"

// store command line configuration.
type Config struct {
	OtlpLoggerName             string
	OtlpExporter               string
	OtlpgRPCURL                string
	OtlpHTTPURL                string
	OtlpTLSCAFile              string
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
	CursorDir       string
	SkipLXCs        bool
	SkipPVE         bool
	//SkipKVMs     	bool
	MonitorInclude []int
	MonitorExclude []int

	DryRun  bool
	Verbose bool
	Version bool
}

// Split and trim comma-separated values.
func splitAndTrim(s string) ([]int, error) {
	ids := []int{}
	parts := strings.Split(s, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		id, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("list item %q must be an integer: %w", part, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func newFlagSet(c *Config, monitorInclude, monitorExclude *string) *flag.FlagSet {
	flags := flag.NewFlagSet("pve2otelcol", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	flags.StringVar(&c.OtlpLoggerName, "otlp-logger-name", DEFAULT_OTLP_LOGGER_NAME, "OpenTelemetry logger name")
	flags.StringVar(&c.OtlpExporter, "otlp-exporter", DEFAULT_OTLP_EXPORTER, "OpenTelemetry exporter (\"grpc\" or \"http\")")
	flags.StringVar(&c.OtlpgRPCURL, "otlp-grpc-url", DEFAULT_OTLP_GRPC_URL, "OpenTelemetry gRPC URL")
	flags.StringVar(&c.OtlpHTTPURL, "otlp-http-url", DEFAULT_OTLP_HTTP_URL, "OpenTelemetry HTTP URL")

	flags.StringVar(&c.OtlpTLSCAFile, "otlp-tls-ca-file", "", "Path to an additional CA certificate file")
	flags.StringVar(&c.OtlpTLSCertFile, "otlp-tls-cert-file", "", "Path to the mutual TLS client certificate file")
	flags.StringVar(&c.OtlpTLSKeyFile, "otlp-tls-key-file", "", "Path to the mutual TLS client key file")
	flags.StringVar(&c.OtlpCompression, "otlp-compression", DEFAULT_OTLP_COMPRESSION,
		"OpenTelemetry compression algorithm (\"gzip\" or \"none\")")
	flags.IntVar(&c.OtlpInitialInterval, "otlp-initial-interval",
		DEFAULT_OTLP_INITIAL_INTERVAL, "OpenTelemetry time to wait after the first failure before retrying in seconds")
	flags.IntVar(&c.OtlpMaxInterval, "otlp-max-interval",
		DEFAULT_OTLP_MAX_INTERVAL, "OpenTelemetry upper bound on backoff interval in seconds")
	flags.IntVar(&c.OtlpMaxElapsedTime, "otlp-max-elapsed-time",
		DEFAULT_OTLP_MAX_ELAPSED_TIME, "OpenTelemetry maximum amount of time (including retries) spent trying to send a request/batch in seconds")
	flags.IntVar(&c.OtlpTimeout, "otlp-timeout",
		DEFAULT_OTLP_TIMEOUT, "OpenTelemetry timeout in milliseconds")

	flags.IntVar(&c.OtlpgRPCReconnectionPeriod, "otlp-grpc-reconnection-period",
		DEFAULT_OTLP_GRPC_RECONNECTION_PERIOD, "OpenTelemetry minimum amount of time between connection attempts to the target endpoint in seconds")

	flags.IntVar(&c.OtlpBatchBufferSize, "otlp-batch-buffer-size",
		DEFAULT_OTLP_BATCH_BUFFER_SIZE, "OpenTelemetry batch buffer size that is kept in memory")
	flags.IntVar(&c.OtlpBatchExportInterval, "otlp-batch-export-interval",
		DEFAULT_OTLP_BATCH_EXPORT_INTERVAL, "OpenTelemetry maximum duration between batched exports in seconds")
	flags.IntVar(&c.OtlpBatchMaxBatchSize, "otlp-batch-max-batch-size",
		DEFAULT_OTLP_BATCH_MAX_BATCH_SIZE, "OpenTelemetry maximum batch size of every export")

	flags.IntVar(&c.RefreshInterval, "refresh-interval", DEFAULT_REFRESH_INTERVAL, "refresh interval in seconds (zero disables periodic refresh)")
	flags.IntVar(&c.CmdRetryTimes, "cmd-retry-times", DEFAULT_CMD_RETRY_TIMES, "number of times a process is restarted before giving up")
	flags.IntVar(&c.CmdRetryDelay, "cmd-retry-delay", DEFAULT_CMD_RETRY_DELAY, "seconds to wait before a process is restarted on failure")
	flags.StringVar(&c.CursorDir, "cursor-dir", DEFAULT_CURSOR_DIR, "directory used to persist journald cursors (empty disables persistence)")
	flags.BoolVar(&c.SkipLXCs, "skip-lxcs", false, "do not monitor LXCs virtuals")
	flags.BoolVar(&c.SkipPVE, "skip-pve", false, "do not monitor this PVE node")
	// It will be reintroduced if we find a way to get the stdout stream from a qm exec command.
	// flags.BoolVar(&c.SkipKVMs, "skip-vms", false, "do not consider Qemu/KVM virtuals")
	flags.StringVar(monitorInclude, "monitor-include", "", "Comma-separated list of IDs to include in monitoring")
	flags.StringVar(monitorExclude, "monitor-exclude", "", "Comma-separated list of IDs to exclude from monitoring")

	flags.BoolVar(&c.DryRun, "dry-run", false, "do not execute any command")
	flags.BoolVar(&c.Verbose, "verbose", false, "be more verbose")
	flags.BoolVar(&c.Version, "version", false, "print version and quit")
	return flags
}

// ParseArgs parses and validates command-line arguments without terminating the process.
func ParseArgs(args []string) (Config, error) {
	c := Config{}
	var monitorInclude string
	var monitorExclude string
	flags := newFlagSet(&c, &monitorInclude, &monitorExclude)
	if err := flags.Parse(args); err != nil {
		return c, err
	}
	if flags.NArg() != 0 {
		return c, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if c.Version {
		return c, nil
	}

	if monitorInclude != "" {
		ids, err := splitAndTrim(monitorInclude)
		if err != nil {
			return c, fmt.Errorf("parse monitor-include: %w", err)
		}
		c.MonitorInclude = ids
	}
	if monitorExclude != "" {
		ids, err := splitAndTrim(monitorExclude)
		if err != nil {
			return c, fmt.Errorf("parse monitor-exclude: %w", err)
		}
		c.MonitorExclude = ids
	}
	for _, id := range c.MonitorInclude {
		if slices.Contains(c.MonitorExclude, id) {
			return c, fmt.Errorf("ID %d is present in both monitor-include and monitor-exclude", id)
		}
	}

	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// PrintUsage writes command-line usage without relying on the process-global FlagSet.
func PrintUsage(w io.Writer) {
	c := Config{}
	var monitorInclude string
	var monitorExclude string
	flags := newFlagSet(&c, &monitorInclude, &monitorExclude)
	flags.SetOutput(w)
	fmt.Fprintf(w, "Usage: %s [options]\n", flags.Name())
	flags.PrintDefaults()
}

// Validate checks configuration values and relationships.
func (c Config) Validate() error {
	if c.OtlpExporter != "grpc" && c.OtlpExporter != "http" {
		return errors.New("otlp-exporter must be \"grpc\" or \"http\"")
	}
	if err := c.validateEndpoint(); err != nil {
		return err
	}
	if err := c.validateTLS(); err != nil {
		return err
	}
	if c.OtlpCompression != "none" && c.OtlpCompression != "gzip" {
		return errors.New("otlp-compression must be \"none\" or \"gzip\"")
	}
	if c.OtlpInitialInterval < 0 {
		return errors.New("otlp-initial-interval must be equal to or greater than zero")
	}
	if c.OtlpMaxInterval < 0 {
		return errors.New("otlp-max-interval must be equal to or greater than zero")
	}
	if c.OtlpInitialInterval > c.OtlpMaxInterval {
		return errors.New("otlp-initial-interval must not exceed otlp-max-interval")
	}
	if c.OtlpMaxElapsedTime < 0 {
		return errors.New("otlp-max-elapsed-time must be equal to or greater than zero")
	}
	if c.OtlpTimeout <= 0 {
		return errors.New("otlp-timeout must be greater than zero")
	}
	if c.OtlpgRPCReconnectionPeriod < 0 {
		return errors.New("otlp-grpc-reconnection-period must be equal to or greater than zero")
	}
	if c.OtlpBatchBufferSize < 1 {
		return errors.New("otlp-batch-buffer-size must be greater than zero")
	}
	if c.OtlpBatchExportInterval < 1 {
		return errors.New("otlp-batch-export-interval must be greater than zero")
	}
	if c.OtlpBatchMaxBatchSize < 1 {
		return errors.New("otlp-batch-max-batch-size must be greater than zero")
	}
	if c.RefreshInterval < 0 {
		return errors.New("refresh-interval must be equal to or greater than zero")
	}
	if c.CmdRetryTimes < 0 {
		return errors.New("cmd-retry-times must be equal to or greater than zero")
	}
	if c.CmdRetryDelay < 0 {
		return errors.New("cmd-retry-delay must be equal to or greater than zero")
	}
	return nil
}

func (c Config) selectedEndpoint() (string, string) {
	if c.OtlpExporter == "http" {
		return "otlp-http-url", c.OtlpHTTPURL
	}
	return "otlp-grpc-url", c.OtlpgRPCURL
}

func (c Config) validateEndpoint() error {
	name, endpoint := c.selectedEndpoint()
	u, err := url.ParseRequestURI(endpoint)
	if err != nil || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) || u.Hostname() == "" {
		return fmt.Errorf("%s must be a valid URL with an http or https scheme and a host", name)
	}
	return nil
}

// validateTLS checks relationships between TLS credentials and the selected
// exporter endpoint. Endpoint syntax beyond the scheme is validated separately.
func (c *Config) validateTLS() error {
	if (c.OtlpTLSCertFile == "") != (c.OtlpTLSKeyFile == "") {
		return errors.New("otlp-tls-cert-file and otlp-tls-key-file must both be specified")
	}

	if c.OtlpTLSCAFile == "" && c.OtlpTLSCertFile == "" {
		return nil
	}

	endpoint := c.OtlpgRPCURL
	if c.OtlpExporter == "http" {
		endpoint = c.OtlpHTTPURL
	}
	u, err := url.Parse(endpoint)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return errors.New("OTLP TLS files require the selected endpoint URL to use https")
	}
	return nil
}
