package pve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alberanid/pve2otelcol/ologgers"
)

func TestMetricsHandlerExposesPrometheusCounters(t *testing.T) {
	p, _, _ := newTestPve(t)
	p.metrics.discoveredSources.Store(3)
	p.metrics.activeMonitors.Store(2)
	p.metrics.monitorRestarts.Store(5)
	p.metrics.journalParseFailures.Store(7)
	p.metrics.oversizedRecords.Store(11)
	p.metrics.droppedRecords.Store(13)
	p.metrics.exporterShutdownFailures.Store(17)

	recorder := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Fatalf("metrics content type = %q", got)
	}
	for _, want := range []string{
		"pve2otelcol_discovered_sources 3",
		"pve2otelcol_active_monitors 2",
		"pve2otelcol_monitor_restarts_total 5",
		"pve2otelcol_journal_parse_failures_total 7",
		"pve2otelcol_oversized_records_total 11",
		"pve2otelcol_dropped_records_total 13",
		"pve2otelcol_exporter_shutdown_failures_total 17",
	} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("metrics body does not contain %q:\n%s", want, recorder.Body.String())
		}
	}
}

func TestSuccessfulDiscoveryUpdatesSourceGauge(t *testing.T) {
	p, _, _ := newTestPve(t)
	p.cfg.DryRun = true
	first := &VM{Id: 301, Type: "lxc", MonitorCmd: "journalctl"}
	second := &VM{Id: 301, Type: "qm", MonitorCmd: "journalctl"}
	p.discoverVMs = func() (VMs, error) {
		return VMs{first.sourceID(): first, second.sourceID(): second}, nil
	}
	if err := p.RefreshVMsMonitoring(); err != nil {
		t.Fatalf("RefreshVMsMonitoring() error = %v", err)
	}
	if got := p.Metrics().DiscoveredSources; got != 2 {
		t.Fatalf("discovered source gauge = %d, want 2", got)
	}
}

func TestExporterShutdownFailureIsCounted(t *testing.T) {
	p, _, _ := newTestPve(t)
	p.shutdownLogger = func(_ context.Context, _ *ologgers.OLogger) error {
		return errors.New("flush failed")
	}
	p.shutdownLoggerWithTimeout(&ologgers.OLogger{}, "lxc/302")
	if got := p.Metrics().ExporterShutdownFailures; got != 1 {
		t.Fatalf("exporter shutdown failure count = %d, want 1", got)
	}
}

func TestMetricsEndpointStartsAndStopsWithService(t *testing.T) {
	p, _, _ := newTestPve(t)
	listener := newBlockingListener()
	p.listen = func(network, address string) (net.Listener, error) {
		if network != "tcp" || address != "127.0.0.1:9221" {
			t.Fatalf("listen call = %q %q", network, address)
		}
		return listener, nil
	}
	p.cfg.MetricsListenAddress = "127.0.0.1:9221"
	p.cfg.SkipPVE = true
	if err := p.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if p.metricsEndpoint == nil {
		t.Fatal("Start() did not create metrics endpoint")
	}
	p.Stop()
	if p.metricsEndpoint != nil {
		t.Fatal("Stop() left metrics endpoint state behind")
	}
	select {
	case <-listener.closed:
	default:
		t.Fatal("Stop() did not close metrics listener")
	}
}

func TestMetricsListenFailurePreventsMonitorStartup(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	p.cfg.MetricsListenAddress = "127.0.0.1:9221"
	listenErr := errors.New("address in use")
	p.listen = func(string, string) (net.Listener, error) { return nil, listenErr }

	err := p.Start()
	if !errors.Is(err, listenErr) {
		t.Fatalf("Start() error = %v, want %v", err, listenErr)
	}
	if loggerStub.calls != 0 || len(runnerStub.calls) != 0 {
		t.Fatalf("monitor startup after listen failure: logger calls=%d runner calls=%d", loggerStub.calls, len(runnerStub.calls))
	}
}

type blockingListener struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingListener() *blockingListener {
	return &blockingListener{closed: make(chan struct{})}
}

func (l *blockingListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *blockingListener) Addr() net.Addr { return testAddr("127.0.0.1:9221") }

type testAddr string

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }
