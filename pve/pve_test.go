package pve

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

type monitorCall struct {
	ctx     context.Context
	vm      *VM
	forever bool
}

type monitorRunnerStub struct {
	calls chan monitorCall
	runFn func(context.Context, *VM, bool) error
}

func (s *monitorRunnerStub) run(ctx context.Context, vm *VM, forever bool) error {
	s.calls <- monitorCall{ctx: ctx, vm: vm, forever: forever}
	if s.runFn != nil {
		return s.runFn(ctx, vm, forever)
	}
	return nil
}

func newTestPve(t *testing.T) (*Pve, *loggerFactoryStub, *monitorRunnerStub) {
	t.Helper()

	p := New(&config.Config{
		SkipLXCs:        true,
		RefreshInterval: 0,
	})
	loggerStub := &loggerFactoryStub{}
	runnerStub := &monitorRunnerStub{calls: make(chan monitorCall, 10)}
	p.newLogger = loggerStub.newLogger
	p.runMonitor = runnerStub.run

	return p, loggerStub, runnerStub
}

func waitForMonitorCall(t *testing.T, runner *monitorRunnerStub) monitorCall {
	t.Helper()
	select {
	case call := <-runner.calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for monitor runner")
		return monitorCall{}
	}
}

func waitForMonitorStopped(t *testing.T, vm *VM) error {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		vm.stateMu.Lock()
		stopped := !vm.running && !vm.stopping
		err := vm.lastError
		vm.stateMu.Unlock()
		if stopped {
			return err
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("timed out waiting for monitor to stop")
			return nil
		}
	}
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
			p, loggerStub, runnerStub := newTestPve(t)
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
			if calls := len(runnerStub.calls); calls != 0 {
				t.Errorf("monitor runner calls = %d, want 0", calls)
			}
		})
	}
}

func TestPVESelfMonitoringLaunchesAfterLoggerCreation(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
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
	call := waitForMonitorCall(t, runnerStub)
	if call.vm == nil {
		t.Fatal("monitor launcher VM = nil, want PVE monitor")
	}
	if call.vm.Logger != loggerStub.logger {
		t.Error("monitor launcher received VM without the created logger")
	}
	if call.vm.Type != "pve" || call.vm.Id != 0 {
		t.Errorf("monitor launcher VM = %s/%d, want pve/0", call.vm.Type, call.vm.Id)
	}
	if !call.forever {
		t.Error("monitor launcher forever = false, want true")
	}

	opts := loggerStub.options[0]
	if opts.ServiceId != "pve/0" {
		t.Errorf("logger service ID = %q, want %q", opts.ServiceId, "pve/0")
	}
	if opts.ServiceName != call.vm.Name {
		t.Errorf("logger service name = %q, want VM name %q", opts.ServiceName, call.vm.Name)
	}
}

func TestStartReturnsPVESelfMonitoringError(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	loggerError := errors.New("exporter initialization failed")
	loggerStub.err = loggerError

	err := p.Start()
	if !errors.Is(err, loggerError) {
		t.Fatalf("Start() error = %v, want wrapped %v", err, loggerError)
	}
	if calls := len(runnerStub.calls); calls != 0 {
		t.Errorf("monitor runner calls = %d, want 0", calls)
	}
}

func TestStartIsIdempotentWithoutRefreshTicker(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}

	if err := p.Start(); err != nil {
		t.Fatalf("first Start() error = %v, want nil", err)
	}
	waitForMonitorCall(t, runnerStub)
	if err := p.Start(); err != nil {
		t.Fatalf("second Start() error = %v, want nil", err)
	}
	if calls := len(runnerStub.calls); calls != 0 {
		t.Errorf("additional monitor runner calls = %d, want 0", calls)
	}
}

func TestStopVMMonitoringCancelsAndWaitsForMonitor(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}
	receivedCancellation := make(chan struct{})
	releaseRunner := make(chan struct{})
	runnerStub.runFn = func(ctx context.Context, _ *VM, _ bool) error {
		<-ctx.Done()
		close(receivedCancellation)
		<-releaseRunner
		return ctx.Err()
	}

	p.StartVMMonitoring(&VM{Id: 101, Name: "test", Type: "lxc", MonitorCmd: "journalctl"})
	call := waitForMonitorCall(t, runnerStub)
	stopReturned := make(chan struct{})
	go func() {
		p.StopVMMonitoring(101)
		close(stopReturned)
	}()

	select {
	case <-receivedCancellation:
	case <-time.After(time.Second):
		t.Fatal("monitor did not receive cancellation")
	}
	select {
	case <-stopReturned:
		t.Fatal("StopVMMonitoring returned before the monitor exited")
	default:
	}

	call.vm.stateMu.Lock()
	running := call.vm.running
	stopping := call.vm.stopping
	call.vm.stateMu.Unlock()
	if running || !stopping {
		t.Errorf("monitor state while stopping = running:%t stopping:%t, want false/true", running, stopping)
	}

	close(releaseRunner)
	select {
	case <-stopReturned:
	case <-time.After(time.Second):
		t.Fatal("StopVMMonitoring did not wait for monitor exit")
	}

	call.vm.stateMu.Lock()
	running = call.vm.running
	stopping = call.vm.stopping
	cancel := call.vm.cancel
	call.vm.stateMu.Unlock()
	if running || stopping || cancel != nil {
		t.Errorf("final monitor state = running:%t stopping:%t cancel:%v, want stopped", running, stopping, cancel)
	}
}

func TestStopVMMonitoringInterruptsRetryDelay(t *testing.T) {
	p, loggerStub, _ := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}
	p.cfg.CmdRetryTimes = 3
	p.cfg.CmdRetryDelay = 60
	p.runMonitor = p.RunKeptAliveProcess
	firstAttempt := make(chan struct{})
	var attempts atomic.Int32
	p.runProcess = func(context.Context, *VM) error {
		if attempts.Add(1) == 1 {
			close(firstAttempt)
		}
		return errors.New("process failed")
	}

	p.StartVMMonitoring(&VM{Id: 102, Name: "retry", Type: "lxc", MonitorCmd: "journalctl"})
	select {
	case <-firstAttempt:
	case <-time.After(time.Second):
		t.Fatal("monitor did not make its first attempt")
	}

	stopReturned := make(chan struct{})
	go func() {
		p.StopVMMonitoring(102)
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
	case <-time.After(time.Second):
		t.Fatal("StopVMMonitoring did not interrupt retry delay")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("process attempts = %d, want 1", got)
	}
}

func TestRetryExhaustionPublishesErrorAndAllowsRestart(t *testing.T) {
	p, loggerStub, _ := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}
	p.cfg.CmdRetryTimes = 2
	p.cfg.CmdRetryDelay = 0
	p.runMonitor = p.RunKeptAliveProcess
	processError := errors.New("process failed")
	var attempts atomic.Int32
	p.runProcess = func(context.Context, *VM) error {
		attempts.Add(1)
		return processError
	}
	vm := &VM{Id: 105, Name: "retry", Type: "lxc", MonitorCmd: "journalctl"}

	p.StartVMMonitoring(vm)
	p.knownVMsMu.RLock()
	stored := p.knownVMs[vm.Id]
	p.knownVMsMu.RUnlock()
	terminalErr := waitForMonitorStopped(t, stored)
	if got := attempts.Load(); got != 3 {
		t.Fatalf("process attempts = %d, want 3 (initial attempt plus two retries)", got)
	}
	if !errors.Is(terminalErr, processError) {
		t.Fatalf("terminal error = %v, want wrapped %v", terminalErr, processError)
	}
	if !strings.Contains(terminalErr.Error(), "after 3 attempt(s)") {
		t.Errorf("terminal error = %q, want attempt count", terminalErr)
	}

	p.StartVMMonitoring(vm)
	terminalErr = waitForMonitorStopped(t, stored)
	if got := attempts.Load(); got != 6 {
		t.Fatalf("process attempts after restart = %d, want 6", got)
	}
	if !errors.Is(terminalErr, processError) {
		t.Fatalf("restart terminal error = %v, want wrapped %v", terminalErr, processError)
	}
}

func TestNormalProcessExitIsRetryableFailure(t *testing.T) {
	p, loggerStub, _ := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}
	p.cfg.CmdRetryTimes = 0
	p.runMonitor = p.RunKeptAliveProcess
	var attempts atomic.Int32
	p.runProcess = func(context.Context, *VM) error {
		attempts.Add(1)
		return nil
	}
	vm := &VM{Id: 106, Name: "exit", Type: "lxc", MonitorCmd: "journalctl"}

	p.StartVMMonitoring(vm)
	p.knownVMsMu.RLock()
	stored := p.knownVMs[vm.Id]
	p.knownVMsMu.RUnlock()
	terminalErr := waitForMonitorStopped(t, stored)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("process attempts = %d, want one initial attempt", got)
	}
	if !errors.Is(terminalErr, errMonitorExited) {
		t.Fatalf("terminal error = %v, want %v", terminalErr, errMonitorExited)
	}
}

func TestDryRunDoesNotCreateLoggersOrMonitorState(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	p.cfg.DryRun = true

	if err := p.pveSelfMonitoring(); err != nil {
		t.Fatalf("pveSelfMonitoring() error = %v, want nil", err)
	}
	p.StartVMMonitoring(&VM{Id: 107, Name: "dry-run", Type: "lxc", MonitorCmd: "journalctl"})
	if loggerStub.calls != 0 {
		t.Errorf("logger factory calls = %d, want 0", loggerStub.calls)
	}
	if calls := len(runnerStub.calls); calls != 0 {
		t.Errorf("monitor runner calls = %d, want 0", calls)
	}
	p.knownVMsMu.RLock()
	knownVMs := len(p.knownVMs)
	p.knownVMsMu.RUnlock()
	if knownVMs != 0 {
		t.Errorf("known VMs = %d, want 0", knownVMs)
	}
	p.lifecycleMu.Lock()
	selfVM := p.selfVM
	p.lifecycleMu.Unlock()
	if selfVM != nil {
		t.Error("self monitor state was created during dry-run")
	}
}

func TestRemoveVMWaitsBeforeDeletingMonitorState(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}
	receivedCancellation := make(chan struct{})
	releaseRunner := make(chan struct{})
	runnerStub.runFn = func(ctx context.Context, _ *VM, _ bool) error {
		<-ctx.Done()
		close(receivedCancellation)
		<-releaseRunner
		return ctx.Err()
	}

	p.StartVMMonitoring(&VM{Id: 104, Name: "remove", Type: "lxc", MonitorCmd: "journalctl"})
	waitForMonitorCall(t, runnerStub)
	removeReturned := make(chan struct{})
	go func() {
		p.RemoveVM(104)
		close(removeReturned)
	}()

	select {
	case <-receivedCancellation:
	case <-time.After(time.Second):
		t.Fatal("monitor did not receive cancellation")
	}
	p.knownVMsMu.RLock()
	_, stillKnown := p.knownVMs[104]
	p.knownVMsMu.RUnlock()
	if !stillKnown {
		t.Fatal("VM state was deleted before the monitor exited")
	}
	select {
	case <-removeReturned:
		t.Fatal("RemoveVM returned before the monitor exited")
	default:
	}

	close(releaseRunner)
	select {
	case <-removeReturned:
	case <-time.After(time.Second):
		t.Fatal("RemoveVM did not return after the monitor exited")
	}
	p.knownVMsMu.RLock()
	_, stillKnown = p.knownVMs[104]
	p.knownVMsMu.RUnlock()
	if stillKnown {
		t.Fatal("VM state remains after monitor removal")
	}
}

func TestStopCancelsAndWaitsForAllMonitors(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}
	cancellations := make(chan int, 2)
	releaseRunners := make(chan struct{})
	runnerStub.runFn = func(ctx context.Context, vm *VM, _ bool) error {
		<-ctx.Done()
		cancellations <- vm.Id
		<-releaseRunners
		return ctx.Err()
	}

	if err := p.pveSelfMonitoring(); err != nil {
		t.Fatalf("pveSelfMonitoring() error = %v, want nil", err)
	}
	p.StartVMMonitoring(&VM{Id: 103, Name: "guest", Type: "lxc", MonitorCmd: "journalctl"})
	waitForMonitorCall(t, runnerStub)
	waitForMonitorCall(t, runnerStub)

	stopReturned := make(chan struct{})
	go func() {
		p.Stop()
		close(stopReturned)
	}()
	for range 2 {
		select {
		case <-cancellations:
		case <-time.After(time.Second):
			t.Fatal("not all monitors received cancellation")
		}
	}
	select {
	case <-stopReturned:
		t.Fatal("Stop returned before all monitors exited")
	default:
	}

	close(releaseRunners)
	select {
	case <-stopReturned:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after all monitors exited")
	}

	secondStopReturned := make(chan struct{})
	go func() {
		p.Stop()
		close(secondStopReturned)
	}()
	select {
	case <-secondStopReturned:
	case <-time.After(time.Second):
		t.Fatal("second Stop call did not return")
	}
}
