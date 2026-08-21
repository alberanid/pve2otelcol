package pve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alberanid/pve2otelcol/config"
	"github.com/alberanid/pve2otelcol/ologgers"
)

// configuration used to monitor a VM
type VM struct {
	Id          int
	Name        string
	Type        string
	MonitorCmd  string
	MonitorArgs []string
	Logger      *ologgers.OLogger

	stateMu        sync.Mutex
	running        bool
	stopping       bool
	removed        bool
	cancel         context.CancelFunc
	done           chan struct{}
	lastError      error
	loggerShutdown bool

	cursorMu                 sync.Mutex
	cursor                   string
	persistedCursor          string
	cursorLoaded             bool
	lastCursorPersistAttempt time.Time
}

// map of VMID to VM information
type VMs map[int]*VM

type loggerFactory func(*config.Config, ologgers.OLoggerOptions) (*ologgers.OLogger, error)

type monitorRunner func(context.Context, *VM, bool) error

type processRunner func(context.Context, *VM) error

type loggerShutdown func(context.Context, *ologgers.OLogger) error

type vmDiscovery func() (VMs, error)

var errMonitorExited = errors.New("monitoring process exited unexpectedly")

const loggerShutdownTimeout = 5 * time.Second
const monitorCommandWaitDelay = 5 * time.Second
const initialJournalRecordBufferSize = 64 * 1024
const maxJournalRecordSize = 1024 * 1024
const maxMonitorStderrSize = 32 * 1024

type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || len(data) > 0
		return written, nil
	}
	if len(data) > remaining {
		b.truncated = true
		data = data[:remaining]
	}
	_, _ = b.buffer.Write(data)
	return written, nil
}

func (b *boundedBuffer) summary() string {
	summary := strings.TrimSpace(b.buffer.String())
	if b.truncated {
		summary += " [truncated]"
	}
	return summary
}

// object used to interact with a Proxmox instance
type Pve struct {
	cfg                    *config.Config
	knownVMs               VMs
	knownVMsMu             sync.RWMutex
	ticker                 ticker
	quitTicker             chan struct{}
	commands               commandRunner
	newProcess             processFactory
	clock                  clock
	newLogger              loggerFactory
	runMonitor             monitorRunner
	runProcess             processRunner
	shutdownLogger         loggerShutdown
	discoverVMs            vmDiscovery
	refreshMu              sync.Mutex
	logRecord              func(*ologgers.OLogger, interface{})
	cursorStore            cursorStore
	capabilitiesMu         sync.Mutex
	journalctlCapabilities map[int]string

	serviceMu   sync.Mutex
	started     bool
	lifecycleMu sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	stopping    bool
	stopDone    chan struct{}
	selfVM      *VM
}

// return a Pve instance.
func New(cfg *config.Config) *Pve {
	ctx, cancel := context.WithCancel(context.Background())
	pve := &Pve{
		cfg:                    cfg,
		knownVMs:               VMs{},
		commands:               execCommandRunner{},
		newProcess:             newExecProcess,
		clock:                  realClock{},
		newLogger:              ologgers.New,
		ctx:                    ctx,
		cancel:                 cancel,
		stopDone:               make(chan struct{}),
		journalctlCapabilities: make(map[int]string),
	}
	pve.runMonitor = pve.RunKeptAliveProcess
	pve.runProcess = pve.runVMMonitoring
	pve.discoverVMs = pve.CurrentVMs
	pve.cursorStore = newCursorStore(cfg.CursorDir)
	pve.logRecord = func(logger *ologgers.OLogger, record interface{}) {
		logger.Log(record)
	}
	pve.shutdownLogger = func(ctx context.Context, logger *ologgers.OLogger) error {
		return logger.Shutdown(ctx)
	}
	return pve
}

func takeLoggerForShutdown(vm *VM) *ologgers.OLogger {
	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()
	if vm.Logger == nil || vm.loggerShutdown {
		return nil
	}
	vm.loggerShutdown = true
	return vm.Logger
}

func (p *Pve) shutdownLoggerWithTimeout(logger *ologgers.OLogger, source string) {
	if logger == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), loggerShutdownTimeout)
	defer cancel()
	if err := p.shutdownLogger(ctx, logger); err != nil {
		slog.Error("unable to shut down OTLP logger", "source", source, "error", err)
	}
}

func (p *Pve) shutdownVMLogger(vm *VM) {
	logger := takeLoggerForShutdown(vm)
	p.shutdownLoggerWithTimeout(logger, fmt.Sprintf("%s/%d", vm.Type, vm.Id))
}

// execute the command to get and parse logs from a VM
func (p *Pve) runVMMonitoring(ctx context.Context, vm *VM) error {
	if err := p.loadCursor(vm); err != nil {
		return err
	}
	defer p.persistCursor(vm, true)

	cmd := p.newProcess(ctx, vm.MonitorCmd, monitorArgsWithCursor(vm)...)
	stderr := &boundedBuffer{limit: maxMonitorStderrSize}
	cmd.SetStderr(stderr)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open monitoring stdout for %s/%d: %w", vm.Type, vm.Id, err)
	}
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("start monitoring command for %s/%d: %w", vm.Type, vm.Id, err)
	}
	seenError := false
	scanner := bufio.NewScanner(stdout)
	// journald fields can contain large payloads. Accept records up to 1 MiB,
	// then fail the attempt instead of silently ending the stream.
	// Scanner also needs room for the line delimiter beyond the record itself.
	scanner.Buffer(make([]byte, initialJournalRecordBufferSize), maxJournalRecordSize+1)
	for scanner.Scan() {
		line := scanner.Text()
		var jData interface{}
		err := json.Unmarshal([]byte(line), &jData)
		if err != nil {
			if !seenError {
				slog.Warn(fmt.Sprintf("failure parsing JSON for %s/%d; some logs will be sent as strings: %s",
					vm.Type, vm.Id, err))
				seenError = true
			}
			p.logRecord(vm.Logger, line)
		} else {
			p.logRecord(vm.Logger, jData)
			p.advanceCursor(vm, jData)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		// A read failure can leave journalctl blocked on a full stdout pipe. Kill
		// the process group before Wait so pct descendants cannot hold it open.
		cancelErr := cmd.Cancel()
		_ = cmd.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := fmt.Errorf("read monitoring output of %s/%d: %w", vm.Type, vm.Id, scanErr)
		if cancelErr != nil && !errors.Is(cancelErr, os.ErrProcessDone) {
			err = errors.Join(err, fmt.Errorf("terminate monitoring command: %w", cancelErr))
		}
		return withMonitorStderr(err, stderr)
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return withMonitorStderr(
			fmt.Errorf("run monitoring command of %s/%d: %w", vm.Type, vm.Id, err),
			stderr,
		)
	}
	return nil
}

func withMonitorStderr(err error, stderr *boundedBuffer) error {
	summary := stderr.summary()
	if summary == "" {
		return err
	}
	return fmt.Errorf("%w; stderr: %q", err, summary)
}

// run a command inside a VM and parse its output that will be sent to a OTLP collector
func (p *Pve) RunKeptAliveProcess(ctx context.Context, vm *VM, forever bool) error {
	if vm.MonitorCmd == "" {
		return errors.New("missing monitoring command")
	}
	strCmd := monitorCommand(vm)
	slog.Debug(fmt.Sprintf("run monitoring process '%s'", strCmd))
	if p.cfg.DryRun {
		slog.Info(fmt.Sprintf("DRY RUN: %s", strCmd))
		return nil
	}
	attempts := 0
	var lastErr error
	for {
		if attempts > 0 {
			// the process failed to run: try again after a delay
			if forever {
				slog.Warn(fmt.Sprintf("command '%s' failed; trying again in %d second(s) (retry %d)",
					strCmd, p.cfg.CmdRetryDelay, attempts), "error", lastErr)
			} else {
				slog.Warn(fmt.Sprintf("command '%s' failed; trying again in %d second(s) (retry %d of %d)",
					strCmd, p.cfg.CmdRetryDelay, attempts, p.cfg.CmdRetryTimes), "error", lastErr)
			}
			timer := p.clock.NewTimer(time.Duration(p.cfg.CmdRetryDelay) * time.Second)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.Chan():
					default:
					}
				}
				return ctx.Err()
			case <-timer.Chan():
			}
		}
		attempts++
		err := p.runProcess(ctx, vm)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			err = errMonitorExited
		}
		lastErr = err
		vm.stateMu.Lock()
		vm.lastError = err
		vm.stateMu.Unlock()
		if !forever && attempts > p.cfg.CmdRetryTimes {
			terminalErr := fmt.Errorf("monitoring of %s/%d failed after %d attempt(s): %w",
				vm.Type, vm.Id, attempts, err)
			slog.Error(terminalErr.Error())
			return terminalErr
		}
	}
}

func monitorCommand(vm *VM) string {
	return strings.TrimSpace(fmt.Sprintf("%s %s", vm.MonitorCmd, strings.Join(vm.MonitorArgs, " ")))
}

func (p *Pve) logDryRunMonitor(vm *VM) {
	slog.Info(fmt.Sprintf("DRY RUN: %s", monitorCommand(vm)))
}

func (p *Pve) startManagedMonitor(vm *VM, forever, self bool) bool {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopping {
		return false
	}

	vm.stateMu.Lock()
	if vm.running || vm.stopping || vm.removed {
		vm.stateMu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(p.ctx)
	done := make(chan struct{})
	vm.running = true
	vm.cancel = cancel
	vm.done = done
	vm.lastError = nil
	vm.stateMu.Unlock()

	if self {
		p.selfVM = vm
	}
	go func() {
		err := p.runMonitor(ctx, vm, forever)
		cancel()
		vm.stateMu.Lock()
		if err != nil && !errors.Is(err, context.Canceled) {
			vm.lastError = err
		}
		vm.running = false
		vm.stopping = false
		vm.cancel = nil
		close(done)
		vm.done = nil
		vm.stateMu.Unlock()
	}()
	return true
}

func requestMonitorStop(vm *VM, remove bool) (context.CancelFunc, <-chan struct{}) {
	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()
	if remove {
		vm.removed = true
	}
	if !vm.running && !vm.stopping {
		return nil, nil
	}
	vm.running = false
	vm.stopping = true
	return vm.cancel, vm.done
}

func stopMonitor(vm *VM, remove bool) {
	cancel, done := requestMonitorStop(vm, remove)
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// monitor Proxmox itself
func (p *Pve) pveSelfMonitoring() error {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "localhost"
	}
	slog.Debug(fmt.Sprintf("start PVE self-monitoring for node %s", hostname))
	vm := VM{
		Id:         0,
		Name:       hostname,
		Type:       "pve",
		MonitorCmd: "journalctl",
		MonitorArgs: []string{
			"--lines",
			"0",
			"--follow",
			"--output",
			"json",
		},
	}
	if p.cfg.DryRun {
		p.logDryRunMonitor(&vm)
		return nil
	}
	logger, err := p.newLogger(p.cfg, ologgers.OLoggerOptions{
		ServiceName: vm.Name,
		ServiceId:   fmt.Sprintf("%s/%d", vm.Type, vm.Id),
	})
	if err != nil {
		p.shutdownLoggerWithTimeout(logger, fmt.Sprintf("%s/%d", vm.Type, vm.Id))
		return fmt.Errorf("create logger for %s/%d: %w", vm.Type, vm.Id, err)
	}
	if logger == nil {
		return fmt.Errorf("create logger for %s/%d: logger factory returned nil", vm.Type, vm.Id)
	}
	vm.Logger = logger
	if !p.startManagedMonitor(&vm, true, true) {
		p.shutdownVMLogger(&vm)
		return fmt.Errorf("start monitor for %s/%d: service is stopping", vm.Type, vm.Id)
	}
	return nil
}

// check whether journalctl is available inside an LXC container
func (p *Pve) lxcHasJournalctl(strID string) (bool, error) {
	ctx, cancel := context.WithTimeout(p.ctx, time.Duration(p.cfg.CapabilityProbeTimeout)*time.Second)
	defer cancel()
	out, err := p.commands.Output(
		ctx,
		"pct", "exec", strID, "--", "sh", "-c",
		"if command -v journalctl >/dev/null 2>&1; then printf yes; else printf no; fi",
	)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return false, fmt.Errorf("timed out after %s: %w", time.Duration(p.cfg.CapabilityProbeTimeout)*time.Second, ctx.Err())
		}
		return false, err
	}
	switch strings.TrimSpace(string(out)) {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected journalctl probe output %q", strings.TrimSpace(string(out)))
	}
}

type pveResource struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Node   string `json:"node"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

func (p *Pve) currentResources() ([]pveResource, error) {
	ctx, cancel := context.WithTimeout(p.ctx, time.Duration(p.cfg.DiscoveryTimeout)*time.Second)
	defer cancel()
	out, err := p.commands.Output(ctx, "pvesh", "get", "/cluster/resources", "--type", "vm", "--output-format", "json")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("resource discovery timed out after %s: %w", time.Duration(p.cfg.DiscoveryTimeout)*time.Second, ctx.Err())
		}
		return nil, fmt.Errorf("list Proxmox resources: %w", err)
	}
	var resources []pveResource
	if err := json.Unmarshal(out, &resources); err != nil {
		return nil, fmt.Errorf("parse Proxmox resource JSON: %w", err)
	}
	for i, resource := range resources {
		if resource.VMID <= 0 || resource.Type == "" || resource.Node == "" {
			return nil, fmt.Errorf("parse Proxmox resource JSON item %d: missing valid vmid, type, or node", i)
		}
	}
	return resources, nil
}

func lxcIdentity(resource pveResource) string {
	return fmt.Sprintf("%s/lxc/%d/%s", resource.Node, resource.VMID, resource.Name)
}

func (p *Pve) cachedLXCJournalctl(id int, identity string) bool {
	p.capabilitiesMu.Lock()
	defer p.capabilitiesMu.Unlock()
	return p.journalctlCapabilities[id] == identity
}

func (p *Pve) cacheLXCJournalctl(id int, identity string) {
	p.capabilitiesMu.Lock()
	p.journalctlCapabilities[id] = identity
	p.capabilitiesMu.Unlock()
}

// check id against the include and exclude lists
func (p *Pve) checkLists(id int) bool {
	if len(p.cfg.MonitorExclude) > 0 && slices.Contains(p.cfg.MonitorExclude, id) {
		return false
	}
	if len(p.cfg.MonitorInclude) > 0 && !slices.Contains(p.cfg.MonitorInclude, id) {
		return false
	}
	return true
}

// return a map containing the currently running LXCs
func (p *Pve) CurrentLXCs() (VMs, error) {
	slog.Debug("updating list of running LXCs")
	vms := VMs{}
	resources, err := p.currentResources()
	if err != nil {
		return nil, err
	}
	node, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("determine local node for LXC discovery: %w", err)
	}
	for _, resource := range resources {
		if resource.Type != "lxc" || resource.Node != node || resource.Status != "running" {
			continue
		}
		if !p.checkLists(resource.VMID) {
			continue
		}
		identity := lxcIdentity(resource)
		if !p.cachedLXCJournalctl(resource.VMID, identity) {
			hasJournalctl, err := p.lxcHasJournalctl(strconv.Itoa(resource.VMID))
			if err != nil {
				return nil, fmt.Errorf("probe lxc/%d for journalctl: %w", resource.VMID, err)
			}
			if !hasJournalctl {
				slog.Debug(fmt.Sprintf("skipping lxc/%d: journalctl not found", resource.VMID))
				continue
			}
			p.cacheLXCJournalctl(resource.VMID, identity)
		}
		strID := strconv.Itoa(resource.VMID)
		vms[resource.VMID] = &VM{
			Id:         resource.VMID,
			Name:       resource.Name,
			Type:       "lxc",
			MonitorCmd: "pct",
			MonitorArgs: []string{
				"exec",
				strID,
				"--",
				"journalctl",
				"--lines",
				"0",
				"--follow",
				"--output",
				"json",
			},
		}
	}
	return vms, nil
}

// return a map containing the currently running KVMs
func (p *Pve) CurrentKVMs() (VMs, error) {
	slog.Debug("updating list of running KVMs")
	vms := VMs{}
	resources, err := p.currentResources()
	if err != nil {
		return nil, err
	}
	node, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("determine local node for KVM discovery: %w", err)
	}
	for _, resource := range resources {
		if resource.Type != "qemu" || resource.Node != node || resource.Status != "running" || !p.checkLists(resource.VMID) {
			continue
		}
		strID := strconv.Itoa(resource.VMID)
		vms[resource.VMID] = &VM{
			Id:         resource.VMID,
			Name:       resource.Name,
			Type:       "qm",
			MonitorCmd: "qm",
			MonitorArgs: []string{
				"exec",
				strID,
				"--",
				"journalctl",
				"--lines",
				"0",
				"--follow",
				"--output",
				"json",
			},
		}
	}
	return vms, nil
}

// return a map containing the currently running LXCs and KVMs
func (p *Pve) CurrentVMs() (VMs, error) {
	vms := VMs{}
	if !p.cfg.SkipLXCs {
		lxcs, err := p.CurrentLXCs()
		if err != nil {
			return nil, err
		}
		maps.Copy(vms, lxcs)
	}
	/*
		// right now KVMs are not monitored, since the qm exec command
		// always block until the command exits, making it impossible to
		// parse the output as a stream.
		if !p.cfg.SkipKVMs {
			kvms, err := p.CurrentKVMs()
			if err != nil {
				return nil, err
			}
			maps.Copy(vms, kvms)
		}
	*/
	return vms, nil
}

// add the received VM to the list of known VMs, creating its logger service if needed
func (p *Pve) UpdateVM(vm *VM) *VM {
	// fast path: check if already present
	p.knownVMsMu.RLock()
	existing, ok := p.knownVMs[vm.Id]
	p.knownVMsMu.RUnlock()
	if ok {
		return existing
	}

	// Hold the lifecycle lock across logger creation and ownership transfer so
	// Stop cannot miss an exporter that is still being constructed.
	p.lifecycleMu.Lock()
	if p.stopping {
		p.lifecycleMu.Unlock()
		return nil
	}
	logger, err := p.newLogger(p.cfg, ologgers.OLoggerOptions{
		ServiceName: vm.Name,
		ServiceId:   fmt.Sprintf("%s/%d", vm.Type, vm.Id),
	})
	if err != nil {
		slog.Warn(fmt.Sprintf("unable to create a logger for %s/%d", vm.Type, vm.Id))
		p.shutdownLoggerWithTimeout(logger, fmt.Sprintf("%s/%d", vm.Type, vm.Id))
		p.lifecycleMu.Unlock()
		return nil
	}
	if logger == nil {
		p.lifecycleMu.Unlock()
		slog.Warn(fmt.Sprintf("unable to create a logger for %s/%d: logger factory returned nil", vm.Type, vm.Id))
		return nil
	}
	vm.Logger = logger

	p.knownVMsMu.Lock()
	if existing, ok := p.knownVMs[vm.Id]; ok {
		p.knownVMsMu.Unlock()
		p.shutdownVMLogger(vm)
		p.lifecycleMu.Unlock()
		return existing
	}
	p.knownVMs[vm.Id] = vm
	p.knownVMsMu.Unlock()
	p.lifecycleMu.Unlock()
	return vm
}

// run the monitoring process of a VM
func (p *Pve) StartVMMonitoring(vm *VM) {
	if p.cfg.DryRun {
		p.logDryRunMonitor(vm)
		return
	}
	// ensure VM is known (and logger created) first
	stored := p.UpdateVM(vm)
	if stored != nil && stored.Logger != nil && p.startManagedMonitor(stored, false, false) {
		slog.Debug(fmt.Sprintf("start monitoring VM %s/%d", stored.Type, stored.Id))
	}
}

// stop the monitoring process of a VM
func (p *Pve) StopVMMonitoring(id int) {
	// obtain vm pointer under read lock, then operate without holding the lock
	p.knownVMsMu.RLock()
	vm, ok := p.knownVMs[id]
	p.knownVMsMu.RUnlock()
	if !ok {
		return
	}
	slog.Debug(fmt.Sprintf("stop monitoring VM %s/%d", vm.Type, vm.Id))
	stopMonitor(vm, false)
}

// remove a VM from the list of known VMs
func (p *Pve) RemoveVM(id int) {
	p.knownVMsMu.RLock()
	vmDesc := fmt.Sprintf("%d", id)
	vm, ok := p.knownVMs[id]
	if ok {
		vmDesc = fmt.Sprintf("%s/%d", vm.Type, id)
	}
	p.knownVMsMu.RUnlock()

	slog.Debug(fmt.Sprintf("remove VM %s", vmDesc))
	if ok {
		stopMonitor(vm, true)
		p.shutdownVMLogger(vm)
	}

	p.knownVMsMu.Lock()
	if current, ok := p.knownVMs[id]; ok && current == vm {
		delete(p.knownVMs, id)
	}
	p.knownVMsMu.Unlock()
}

// refresh the map of running VMs
func (p *Pve) RefreshVMsMonitoring() error {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	vms, err := p.discoverVMs()
	if err != nil {
		return fmt.Errorf("discover current VMs: %w", err)
	}
	for _, vm := range vms {
		p.StartVMMonitoring(vm)
	}

	remove := []int{}
	p.knownVMsMu.RLock()
	for id, vm := range p.knownVMs {
		if _, ok := vms[id]; !ok {
			remove = append(remove, vm.Id)
		}
	}
	p.knownVMsMu.RUnlock()

	for _, id := range remove {
		p.RemoveVM(id)
	}
	return nil
}

func (p *Pve) refreshVMsMonitoringAndLog() {
	if err := p.RefreshVMsMonitoring(); err != nil {
		slog.Error("unable to refresh VM monitoring", "error", err)
	}
}

func (p *Pve) periodicRefresh() {
	p.refreshVMsMonitoringAndLog()
	if p.cfg.RefreshInterval == 0 {
		return
	}
	p.ticker = p.clock.NewTicker(time.Duration(p.cfg.RefreshInterval) * time.Second)
	quitTicker := make(chan struct{})
	p.quitTicker = quitTicker
	go func() {
		for {
			select {
			case <-quitTicker:
				// was asked to stop
				return
			case <-p.ticker.Chan():
				// periodic task
				p.refreshVMsMonitoringAndLog()
			}
		}
	}()
}

// start managing monitoring processes
func (p *Pve) Start() error {
	p.serviceMu.Lock()
	defer p.serviceMu.Unlock()
	if p.started {
		// do nothing, if already running
		return nil
	}
	p.lifecycleMu.Lock()
	stopping := p.stopping
	p.lifecycleMu.Unlock()
	if stopping {
		return errors.New("service is stopping")
	}
	slog.Info("start monitoring")
	if !p.cfg.SkipPVE {
		if err := p.pveSelfMonitoring(); err != nil {
			return err
		}
	}
	p.periodicRefresh()
	p.started = true
	return nil
}

// stop all running monitoring processes
func (p *Pve) Stop() {
	p.serviceMu.Lock()
	defer p.serviceMu.Unlock()
	slog.Info("stop monitoring")
	p.lifecycleMu.Lock()
	if p.stopping {
		done := p.stopDone
		p.lifecycleMu.Unlock()
		<-done
		return
	}
	p.stopping = true
	selfVM := p.selfVM
	stopDone := p.stopDone
	p.lifecycleMu.Unlock()

	if p.ticker != nil {
		p.ticker.Stop()
	}
	if p.quitTicker != nil {
		close(p.quitTicker)
		p.quitTicker = nil
	}

	// collect ids first under lock to avoid holding lock while removing
	ids := []int{}
	p.knownVMsMu.RLock()
	for id := range p.knownVMs {
		ids = append(ids, id)
	}
	p.knownVMsMu.RUnlock()

	type pendingStop struct {
		cancel context.CancelFunc
		done   <-chan struct{}
		vm     *VM
	}
	pending := make([]pendingStop, 0, len(ids)+1)
	if selfVM != nil {
		cancel, done := requestMonitorStop(selfVM, true)
		pending = append(pending, pendingStop{cancel: cancel, done: done, vm: selfVM})
	}
	for _, id := range ids {
		p.knownVMsMu.RLock()
		vm := p.knownVMs[id]
		p.knownVMsMu.RUnlock()
		if vm != nil {
			cancel, done := requestMonitorStop(vm, true)
			pending = append(pending, pendingStop{cancel: cancel, done: done, vm: vm})
		}
	}

	p.cancel()
	for _, monitor := range pending {
		if monitor.cancel != nil {
			monitor.cancel()
		}
	}
	for _, monitor := range pending {
		if monitor.done != nil {
			<-monitor.done
		}
	}

	var shutdownWG sync.WaitGroup
	for _, monitor := range pending {
		if monitor.vm == nil {
			continue
		}
		shutdownWG.Add(1)
		go func(vm *VM) {
			defer shutdownWG.Done()
			p.shutdownVMLogger(vm)
		}(monitor.vm)
	}
	shutdownWG.Wait()

	p.knownVMsMu.Lock()
	for _, id := range ids {
		delete(p.knownVMs, id)
	}
	p.knownVMsMu.Unlock()

	close(stopDone)
}
