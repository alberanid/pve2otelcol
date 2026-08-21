package pve

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alberanid/pve2otelcol/config"
	"github.com/alberanid/pve2otelcol/ologgers"
)

type loggerFactoryStub struct {
	logger      *ologgers.OLogger
	err         error
	calls       int
	options     []ologgers.OLoggerOptions
	newLoggerFn func(ologgers.OLoggerOptions) (*ologgers.OLogger, error)
}

func (s *loggerFactoryStub) newLogger(_ *config.Config, opts ologgers.OLoggerOptions) (*ologgers.OLogger, error) {
	s.calls++
	s.options = append(s.options, opts)
	if s.newLoggerFn != nil {
		return s.newLoggerFn(opts)
	}
	return s.logger, s.err
}

type loggerShutdownSpy struct {
	mu                   sync.Mutex
	calls                map[*ologgers.OLogger]int
	allContextsDeadlined bool
}

func newLoggerShutdownSpy() *loggerShutdownSpy {
	return &loggerShutdownSpy{
		calls:                make(map[*ologgers.OLogger]int),
		allContextsDeadlined: true,
	}
}

func (s *loggerShutdownSpy) shutdown(ctx context.Context, logger *ologgers.OLogger) error {
	_, hasDeadline := ctx.Deadline()
	s.mu.Lock()
	s.calls[logger]++
	s.allContextsDeadlined = s.allContextsDeadlined && hasDeadline
	s.mu.Unlock()
	return nil
}

func (s *loggerShutdownSpy) count(logger *ologgers.OLogger) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[logger]
}

func (s *loggerShutdownSpy) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, calls := range s.calls {
		total += calls
	}
	return total
}

func (s *loggerShutdownSpy) contextsHaveDeadlines() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allContextsDeadlined
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
		SkipLXCs:               true,
		RefreshInterval:        0,
		DiscoveryTimeout:       config.DEFAULT_DISCOVERY_TIMEOUT,
		CapabilityProbeTimeout: config.DEFAULT_CAPABILITY_PROBE_TIMEOUT,
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
		name         string
		logger       *ologgers.OLogger
		loggerErr    error
		wantErr      error
		wantErrMsg   string
		wantShutdown int
	}{
		{
			name:      "logger factory returns error",
			loggerErr: loggerError,
			wantErr:   loggerError,
		},
		{
			name:         "logger factory returns partial logger and error",
			logger:       &ologgers.OLogger{},
			loggerErr:    loggerError,
			wantErr:      loggerError,
			wantShutdown: 1,
		},
		{
			name:       "logger factory returns nil logger",
			wantErrMsg: "logger factory returned nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, loggerStub, runnerStub := newTestPve(t)
			shutdownSpy := newLoggerShutdownSpy()
			p.shutdownLogger = shutdownSpy.shutdown
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
			if calls := shutdownSpy.count(tt.logger); calls != tt.wantShutdown {
				t.Errorf("logger shutdown calls = %d, want %d", calls, tt.wantShutdown)
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

func TestMonitorCommandCancellationKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	vm := &VM{
		MonitorCmd:  "/bin/sh",
		MonitorArgs: []string{"-c", "sleep 60 & echo ready; wait"},
	}
	process := newExecProcess(ctx, vm.MonitorCmd, monitorArgsWithCursor(vm)...)
	cmd := process.(*execProcess).cmd
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("process readiness = %q, %v; want ready", line, err)
	}
	processGroupID := cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, reader)
		_ = cmd.Wait()
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor process group did not exit after cancellation")
	}

	deadline := time.Now().Add(time.Second)
	for {
		err := syscall.Kill(-processGroupID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("monitor process group %d still exists after cancellation: %v", processGroupID, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStopDoesNotBlockOnTickerNotification(t *testing.T) {
	p, _, _ := newTestPve(t)
	p.ticker = realTicker{Ticker: time.NewTicker(time.Hour)}
	p.quitTicker = make(chan struct{})
	stopReturned := make(chan struct{})
	go func() {
		p.Stop()
		close(stopReturned)
	}()

	select {
	case <-stopReturned:
	case <-time.After(time.Second):
		t.Fatal("Stop blocked notifying a ticker with no active receiver")
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

func TestStopWaitsForInFlightLoggerCreation(t *testing.T) {
	p, _, _ := newTestPve(t)
	shutdownSpy := newLoggerShutdownSpy()
	p.shutdownLogger = shutdownSpy.shutdown
	logger := &ologgers.OLogger{}
	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	p.newLogger = func(*config.Config, ologgers.OLoggerOptions) (*ologgers.OLogger, error) {
		close(factoryEntered)
		<-releaseFactory
		return logger, nil
	}

	startReturned := make(chan struct{})
	go func() {
		p.StartVMMonitoring(&VM{Id: 109, Name: "starting", Type: "lxc", MonitorCmd: "journalctl"})
		close(startReturned)
	}()
	select {
	case <-factoryEntered:
	case <-time.After(time.Second):
		t.Fatal("logger factory did not start")
	}
	stopReturned := make(chan struct{})
	go func() {
		p.Stop()
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
		t.Fatal("Stop returned while logger creation was in progress")
	default:
	}

	close(releaseFactory)
	select {
	case <-startReturned:
	case <-time.After(time.Second):
		t.Fatal("StartVMMonitoring did not return")
	}
	select {
	case <-stopReturned:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after logger creation completed")
	}
	if calls := shutdownSpy.count(logger); calls != 1 {
		t.Errorf("logger shutdown calls = %d, want 1", calls)
	}
}

func TestRemoveVMWaitsBeforeDeletingMonitorState(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}
	shutdownSpy := newLoggerShutdownSpy()
	p.shutdownLogger = shutdownSpy.shutdown
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
	if calls := shutdownSpy.count(loggerStub.logger); calls != 0 {
		t.Fatalf("logger shutdown calls before monitor exit = %d, want 0", calls)
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
	if calls := shutdownSpy.count(loggerStub.logger); calls != 1 {
		t.Errorf("logger shutdown calls = %d, want 1", calls)
	}
	if !shutdownSpy.contextsHaveDeadlines() {
		t.Error("logger shutdown context has no deadline")
	}
}

func TestStopCancelsAndWaitsForAllMonitors(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	loggerStub.newLoggerFn = func(ologgers.OLoggerOptions) (*ologgers.OLogger, error) {
		return &ologgers.OLogger{}, nil
	}
	shutdownSpy := newLoggerShutdownSpy()
	p.shutdownLogger = shutdownSpy.shutdown
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
	if calls := shutdownSpy.total(); calls != 0 {
		t.Fatalf("logger shutdown calls before monitors exit = %d, want 0", calls)
	}

	close(releaseRunners)
	select {
	case <-stopReturned:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after all monitors exited")
	}
	if calls := shutdownSpy.total(); calls != 2 {
		t.Errorf("logger shutdown calls = %d, want 2", calls)
	}
	if !shutdownSpy.contextsHaveDeadlines() {
		t.Error("one or more logger shutdown contexts have no deadline")
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
	if calls := shutdownSpy.total(); calls != 2 {
		t.Errorf("logger shutdown calls after second Stop = %d, want 2", calls)
	}
}

func TestRefreshDiscoveryFailurePreservesRunningMonitor(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}
	shutdownSpy := newLoggerShutdownSpy()
	p.shutdownLogger = shutdownSpy.shutdown

	monitorCanceled := make(chan struct{})
	runnerStub.runFn = func(ctx context.Context, _ *VM, _ bool) error {
		<-ctx.Done()
		close(monitorCanceled)
		return ctx.Err()
	}

	vm := &VM{Id: 110, Name: "existing", Type: "lxc", MonitorCmd: "journalctl"}
	p.StartVMMonitoring(vm)
	waitForMonitorCall(t, runnerStub)

	discoveryErr := errors.New("pct temporarily unavailable")
	p.discoverVMs = func() (VMs, error) {
		return nil, discoveryErr
	}

	err := p.RefreshVMsMonitoring()
	if !errors.Is(err, discoveryErr) {
		t.Fatalf("RefreshVMsMonitoring() error = %v, want wrapped %v", err, discoveryErr)
	}

	p.knownVMsMu.RLock()
	stored, ok := p.knownVMs[vm.Id]
	p.knownVMsMu.RUnlock()
	if !ok || stored != vm {
		t.Fatal("discovery failure removed the existing monitor")
	}
	select {
	case <-monitorCanceled:
		t.Fatal("discovery failure canceled the existing monitor")
	default:
	}
	if got := shutdownSpy.count(vm.Logger); got != 0 {
		t.Fatalf("logger shutdown count after discovery failure = %d, want 0", got)
	}

	p.Stop()
}

func TestRefreshSuccessfulEmptySnapshotRemovesMonitor(t *testing.T) {
	p, loggerStub, runnerStub := newTestPve(t)
	loggerStub.logger = &ologgers.OLogger{}
	shutdownSpy := newLoggerShutdownSpy()
	p.shutdownLogger = shutdownSpy.shutdown

	monitorCanceled := make(chan struct{})
	runnerStub.runFn = func(ctx context.Context, _ *VM, _ bool) error {
		<-ctx.Done()
		close(monitorCanceled)
		return ctx.Err()
	}

	vm := &VM{Id: 111, Name: "removed", Type: "lxc", MonitorCmd: "journalctl"}
	p.StartVMMonitoring(vm)
	waitForMonitorCall(t, runnerStub)
	p.discoverVMs = func() (VMs, error) {
		return VMs{}, nil
	}

	if err := p.RefreshVMsMonitoring(); err != nil {
		t.Fatalf("RefreshVMsMonitoring() error = %v", err)
	}
	select {
	case <-monitorCanceled:
	default:
		t.Fatal("successful empty discovery did not cancel the monitor")
	}
	p.knownVMsMu.RLock()
	_, ok := p.knownVMs[vm.Id]
	p.knownVMsMu.RUnlock()
	if ok {
		t.Fatal("successful empty discovery left the monitor tracked")
	}
	if got := shutdownSpy.count(vm.Logger); got != 1 {
		t.Fatalf("logger shutdown count = %d, want 1", got)
	}

	p.Stop()
}

func TestRefreshesAreSerialized(t *testing.T) {
	p, _, _ := newTestPve(t)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	var calls atomic.Int32
	p.discoverVMs = func() (VMs, error) {
		switch calls.Add(1) {
		case 1:
			close(firstEntered)
			<-releaseFirst
		case 2:
			close(secondEntered)
		}
		return VMs{}, nil
	}

	errs := make(chan error, 2)
	go func() { errs <- p.RefreshVMsMonitoring() }()
	<-firstEntered
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		errs <- p.RefreshVMsMonitoring()
	}()
	<-secondStarted

	select {
	case <-secondEntered:
		t.Fatal("second discovery overlapped the first refresh")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("RefreshVMsMonitoring() error = %v", err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("discovery calls = %d, want 2", got)
	}

	p.Stop()
}

func TestRunVMMonitoringAcceptsLargeJournalRecord(t *testing.T) {
	p, _, _ := newTestPve(t)
	const payloadSize = 128 * 1024
	var captured interface{}
	p.logRecord = func(_ *ologgers.OLogger, record interface{}) {
		captured = record
	}
	vm := &VM{
		Id:         120,
		Type:       "lxc",
		MonitorCmd: "/bin/sh",
		MonitorArgs: []string{
			"-c",
			"printf '\"'; head -c 131072 /dev/zero | tr '\\000' x; printf '\"\\n'",
		},
	}

	if err := p.runVMMonitoring(context.Background(), vm); err != nil {
		t.Fatalf("runVMMonitoring() error = %v", err)
	}
	got, ok := captured.(string)
	if !ok {
		t.Fatalf("captured record type = %T, want string", captured)
	}
	if len(got) != payloadSize {
		t.Fatalf("captured payload length = %d, want %d", len(got), payloadSize)
	}
}

func TestRunVMMonitoringRejectsRecordOverLimitAndTerminatesChild(t *testing.T) {
	p, _, _ := newTestPve(t)
	var logged atomic.Int32
	p.logRecord = func(_ *ologgers.OLogger, _ interface{}) {
		logged.Add(1)
	}
	vm := &VM{
		Id:         121,
		Type:       "lxc",
		MonitorCmd: "/bin/sh",
		MonitorArgs: []string{
			"-c",
			"head -c 1048577 /dev/zero | tr '\\000' x; sleep 60",
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- p.runVMMonitoring(context.Background(), vm)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "read monitoring output of lxc/121") {
			t.Fatalf("runVMMonitoring() error = %v, want record-size read error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runVMMonitoring blocked after oversized record")
	}
	if got := logged.Load(); got != 0 {
		t.Fatalf("records logged over the size limit = %d, want 0", got)
	}
}

func TestRunVMMonitoringFallsBackToStringForMalformedJSON(t *testing.T) {
	p, _, _ := newTestPve(t)
	var captured interface{}
	p.logRecord = func(_ *ologgers.OLogger, record interface{}) {
		captured = record
	}
	vm := &VM{
		Id:          122,
		Type:        "lxc",
		MonitorCmd:  "/bin/sh",
		MonitorArgs: []string{"-c", "printf 'not-json\\n'"},
	}

	if err := p.runVMMonitoring(context.Background(), vm); err != nil {
		t.Fatalf("runVMMonitoring() error = %v", err)
	}
	if got, ok := captured.(string); !ok || got != "not-json" {
		t.Fatalf("captured record = %#v, want malformed line as string", captured)
	}
}

func TestRunVMMonitoringReturnsNilAtCleanEOF(t *testing.T) {
	p, _, _ := newTestPve(t)
	logged := 0
	p.logRecord = func(_ *ologgers.OLogger, _ interface{}) {
		logged++
	}
	vm := &VM{
		Id:          123,
		Type:        "lxc",
		MonitorCmd:  "/bin/sh",
		MonitorArgs: []string{"-c", "printf '{\"MESSAGE\":\"done\"}\\n'"},
	}

	if err := p.runVMMonitoring(context.Background(), vm); err != nil {
		t.Fatalf("runVMMonitoring() error = %v", err)
	}
	if logged != 1 {
		t.Fatalf("logged records = %d, want 1", logged)
	}
}

func TestRunVMMonitoringReturnsContextErrorOnCancellation(t *testing.T) {
	p, _, _ := newTestPve(t)
	recordLogged := make(chan struct{})
	p.logRecord = func(_ *ologgers.OLogger, _ interface{}) {
		close(recordLogged)
	}
	vm := &VM{
		Id:          124,
		Type:        "lxc",
		MonitorCmd:  "/bin/sh",
		MonitorArgs: []string{"-c", "printf '{\"MESSAGE\":\"ready\"}\\n'; sleep 60"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- p.runVMMonitoring(ctx, vm)
	}()
	select {
	case <-recordLogged:
	case <-time.After(time.Second):
		t.Fatal("monitor command did not become ready")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runVMMonitoring() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runVMMonitoring did not return after cancellation")
	}
}

func TestRunVMMonitoringIncludesBoundedStderrOnChildError(t *testing.T) {
	p, _, _ := newTestPve(t)
	p.logRecord = func(_ *ologgers.OLogger, _ interface{}) {}
	vm := &VM{
		Id:         125,
		Type:       "lxc",
		MonitorCmd: "/bin/sh",
		MonitorArgs: []string{
			"-c",
			"{ printf 'diagnostic: '; head -c 65536 /dev/zero | tr '\\000' e; } >&2; exit 7",
		},
	}

	err := p.runVMMonitoring(context.Background(), vm)
	if err == nil {
		t.Fatal("runVMMonitoring() error = nil, want child-process error")
	}
	message := err.Error()
	if !strings.Contains(message, "diagnostic:") || !strings.Contains(message, "[truncated]") {
		t.Fatalf("runVMMonitoring() error lacks bounded stderr details: %v", err)
	}
	if len(message) > maxMonitorStderrSize+512 {
		t.Fatalf("runVMMonitoring() error length = %d, stderr was not bounded", len(message))
	}
}
