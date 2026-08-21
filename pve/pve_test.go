package pve

import (
	"errors"
	"strings"
	"testing"

	"github.com/alberanid/pve2otelcol/config"
	"github.com/alberanid/pve2otelcol/ologgers"
)

type loggerFactoryStub struct {
	logger  *ologgers.OLogger
	err     error
	calls   int
	options []ologgers.OLoggerOptions
}

func (s *loggerFactoryStub) newLogger(_ *config.Config, opts ologgers.OLoggerOptions) (*ologgers.OLogger, error) {
	s.calls++
	s.options = append(s.options, opts)
	return s.logger, s.err
}

type monitorLauncherSpy struct {
	calls   int
	vm      *VM
	forever bool
}

func (s *monitorLauncherSpy) launch(vm *VM, forever bool) {
	s.calls++
	s.vm = vm
	s.forever = forever
}

func newTestPve(t *testing.T) (*Pve, *loggerFactoryStub, *monitorLauncherSpy) {
	t.Helper()

	p := New(&config.Config{
		SkipLXCs:        true,
		RefreshInterval: 0,
	})
	loggerStub := &loggerFactoryStub{}
	launcherSpy := &monitorLauncherSpy{}
	p.newLogger = loggerStub.newLogger
	p.launchMonitor = launcherSpy.launch

	return p, loggerStub, launcherSpy
}

func TestPVESelfMonitoringDoesNotLaunchWithoutLogger(t *testing.T) {
	loggerError := errors.New("exporter initialization failed")
	tests := []struct {
		name       string
		logger     *ologgers.OLogger
		loggerErr  error
		wantErr    error
		wantErrMsg string
	}{
		{
			name:      "logger factory returns error",
			loggerErr: loggerError,
			wantErr:   loggerError,
		},
		{
			name:       "logger factory returns nil logger",
			wantErrMsg: "logger factory returned nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, loggerStub, launcherSpy := newTestPve(t)
			loggerStub.logger = tt.logger
			loggerStub.err = tt.loggerErr

			err := p.pveSelfMonitoring()
			if err == nil {
				t.Fatal("pveSelfMonitoring() error = nil, want non-nil")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("pveSelfMonitoring() error = %v, want wrapped %v", err, tt.wantErr)
			}
			if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Fatalf("pveSelfMonitoring() error = %q, want it to contain %q", err, tt.wantErrMsg)
			}
			if loggerStub.calls != 1 {
				t.Errorf("logger factory calls = %d, want 1", loggerStub.calls)
			}
			if launcherSpy.calls != 0 {
				t.Errorf("monitor launcher calls = %d, want 0", launcherSpy.calls)
			}
		})
	}
}

func TestPVESelfMonitoringLaunchesAfterLoggerCreation(t *testing.T) {
	p, loggerStub, launcherSpy := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}

	if err := p.pveSelfMonitoring(); err != nil {
		t.Fatalf("pveSelfMonitoring() error = %v, want nil", err)
	}

	if loggerStub.calls != 1 {
		t.Fatalf("logger factory calls = %d, want 1", loggerStub.calls)
	}
	if len(loggerStub.options) != 1 {
		t.Fatalf("logger factory options = %d, want 1", len(loggerStub.options))
	}
	if launcherSpy.calls != 1 {
		t.Fatalf("monitor launcher calls = %d, want 1", launcherSpy.calls)
	}
	if launcherSpy.vm == nil {
		t.Fatal("monitor launcher VM = nil, want PVE monitor")
	}
	if launcherSpy.vm.Logger != loggerStub.logger {
		t.Error("monitor launcher received VM without the created logger")
	}
	if launcherSpy.vm.Type != "pve" || launcherSpy.vm.Id != 0 {
		t.Errorf("monitor launcher VM = %s/%d, want pve/0", launcherSpy.vm.Type, launcherSpy.vm.Id)
	}
	if !launcherSpy.forever {
		t.Error("monitor launcher forever = false, want true")
	}

	opts := loggerStub.options[0]
	if opts.ServiceId != "pve/0" {
		t.Errorf("logger service ID = %q, want %q", opts.ServiceId, "pve/0")
	}
	if opts.ServiceName != launcherSpy.vm.Name {
		t.Errorf("logger service name = %q, want VM name %q", opts.ServiceName, launcherSpy.vm.Name)
	}
}

func TestStartReturnsPVESelfMonitoringError(t *testing.T) {
	p, loggerStub, launcherSpy := newTestPve(t)
	loggerError := errors.New("exporter initialization failed")
	loggerStub.err = loggerError

	err := p.Start()
	if !errors.Is(err, loggerError) {
		t.Fatalf("Start() error = %v, want wrapped %v", err, loggerError)
	}
	if launcherSpy.calls != 0 {
		t.Errorf("monitor launcher calls = %d, want 0", launcherSpy.calls)
	}
}
