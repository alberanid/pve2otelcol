package pve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// MetricsSnapshot is a race-free point-in-time view of service health.
type MetricsSnapshot struct {
	DiscoveredSources        int64
	ActiveMonitors           int64
	MonitorRestarts          uint64
	JournalParseFailures     uint64
	OversizedRecords         uint64
	DroppedRecords           uint64
	ExporterShutdownFailures uint64
}

type metrics struct {
	discoveredSources        atomic.Int64
	activeMonitors           atomic.Int64
	monitorRestarts          atomic.Uint64
	journalParseFailures     atomic.Uint64
	oversizedRecords         atomic.Uint64
	droppedRecords           atomic.Uint64
	exporterShutdownFailures atomic.Uint64
}

type metricsEndpoint struct {
	listener net.Listener
	server   *http.Server
}

func (m *metrics) snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		DiscoveredSources:        m.discoveredSources.Load(),
		ActiveMonitors:           m.activeMonitors.Load(),
		MonitorRestarts:          m.monitorRestarts.Load(),
		JournalParseFailures:     m.journalParseFailures.Load(),
		OversizedRecords:         m.oversizedRecords.Load(),
		DroppedRecords:           m.droppedRecords.Load(),
		ExporterShutdownFailures: m.exporterShutdownFailures.Load(),
	}
}

func (m *metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s := m.snapshot()
	writeMetric(w, "pve2otelcol_discovered_sources", "Sources in the latest successful discovery snapshot.", "gauge", s.DiscoveredSources)
	writeMetric(w, "pve2otelcol_active_monitors", "Currently running PVE and guest monitor workers.", "gauge", s.ActiveMonitors)
	writeMetric(w, "pve2otelcol_monitor_restarts_total", "Additional monitor process starts after the initial attempt.", "counter", s.MonitorRestarts)
	writeMetric(w, "pve2otelcol_journal_parse_failures_total", "Journal records that could not be decoded as JSON and were forwarded as strings.", "counter", s.JournalParseFailures)
	writeMetric(w, "pve2otelcol_oversized_records_total", "Journal records rejected for exceeding the configured safety limit.", "counter", s.OversizedRecords)
	writeMetric(w, "pve2otelcol_dropped_records_total", "Journal records not handed to the OpenTelemetry logger.", "counter", s.DroppedRecords)
	writeMetric(w, "pve2otelcol_exporter_shutdown_failures_total", "OpenTelemetry logger providers that failed to shut down cleanly.", "counter", s.ExporterShutdownFailures)
}

func writeMetric(w io.Writer, name, help, metricType string, value interface{}) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, metricType, name, value)
}

// Metrics returns a point-in-time snapshot suitable for tests and embedding.
func (p *Pve) Metrics() MetricsSnapshot {
	return p.metrics.snapshot()
}

// MetricsHandler exposes Prometheus text-format service metrics.
func (p *Pve) MetricsHandler() http.Handler {
	return newMetricsHandler(p.metrics)
}

func newMetricsHandler(m *metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m)
	return mux
}

func (p *Pve) startMetricsEndpoint() error {
	if p.cfg.MetricsListenAddress == "" {
		return nil
	}
	listener, err := p.listen("tcp", p.cfg.MetricsListenAddress)
	if err != nil {
		return fmt.Errorf("listen for metrics on %s: %w", p.cfg.MetricsListenAddress, err)
	}
	server := &http.Server{
		Handler:           newMetricsHandler(p.metrics),
		ReadHeaderTimeout: 5 * time.Second,
	}
	p.metricsEndpoint = &metricsEndpoint{listener: listener, server: server}
	slog.Info("metrics endpoint listening", "address", listener.Addr().String())
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed && !errors.Is(err, net.ErrClosed) {
			slog.Error("metrics endpoint failed", "address", listener.Addr().String(), "error", err)
		}
	}()
	return nil
}

func (p *Pve) stopMetricsEndpoint() {
	endpoint := p.metricsEndpoint
	if endpoint == nil {
		return
	}
	p.metricsEndpoint = nil
	ctx, cancel := context.WithTimeout(context.Background(), loggerShutdownTimeout)
	defer cancel()
	if err := endpoint.server.Shutdown(ctx); err != nil {
		slog.Error("unable to shut down metrics endpoint", "address", endpoint.listener.Addr().String(), "error", err)
	}
	if err := endpoint.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Error("unable to close metrics listener", "address", endpoint.listener.Addr().String(), "error", err)
	}
}
