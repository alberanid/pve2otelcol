package pve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
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

	stateMu   sync.Mutex
	running   bool
	stopping  bool
	removed   bool
	cancel    context.CancelFunc
	done      chan struct{}
	lastError error
}

// map of VMID to VM information
type VMs map[int]*VM

type loggerFactory func(*config.Config, ologgers.OLoggerOptions) (*ologgers.OLogger, error)

type monitorRunner func(context.Context, *VM, bool) error

type processRunner func(context.Context, *VM) error

var errMonitorExited = errors.New("monitoring process exited unexpectedly")

// object used to interact with a Proxmox instance
type Pve struct {
	cfg        *config.Config
	knownVMs   VMs
	knownVMsMu sync.RWMutex
	ticker     *time.Ticker
	quitTicker chan bool
	newLogger  loggerFactory
	runMonitor monitorRunner
	runProcess processRunner

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
		cfg:       cfg,
		knownVMs:  VMs{},
		newLogger: ologgers.New,
		ctx:       ctx,
		cancel:    cancel,
		stopDone:  make(chan struct{}),
	}
	pve.runMonitor = pve.RunKeptAliveProcess
	pve.runProcess = pve.runVMMonitoring
	return pve
}

// execute the command to get and parse logs from a VM
func (p *Pve) runVMMonitoring(ctx context.Context, vm *VM) error {
	cmd := exec.CommandContext(ctx, vm.MonitorCmd, vm.MonitorArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Error(fmt.Sprintf("failure opening standard output of %s/%d: %v", vm.Type, vm.Id, err))
		return err
	}
	err = cmd.Start()
	if err != nil {
		slog.Error(fmt.Sprintf("failure starting monitoring command of %s/%d: %v", vm.Type, vm.Id, err))
		return err
	}
	seenError := false
	scanner := bufio.NewScanner(stdout)
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
			vm.Logger.Log(line)
		} else {
			vm.Logger.Log(jData)
		}
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		slog.Error(fmt.Sprintf("failure running monitoring command of %s/%d: %v", vm.Type, vm.Id, err))
	}
	return err
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
	for {
		if attempts > 0 {
			// the process failed to run: try again after a delay
			if forever {
				slog.Warn(fmt.Sprintf("command '%s' failed; trying again in %d second(s) (retry %d)",
					strCmd, p.cfg.CmdRetryDelay, attempts))
			} else {
				slog.Warn(fmt.Sprintf("command '%s' failed; trying again in %d second(s) (retry %d of %d)",
					strCmd, p.cfg.CmdRetryDelay, attempts, p.cfg.CmdRetryTimes))
			}
			timer := time.NewTimer(time.Duration(p.cfg.CmdRetryDelay) * time.Second)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return ctx.Err()
			case <-timer.C:
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
		return fmt.Errorf("create logger for %s/%d: %w", vm.Type, vm.Id, err)
	}
	if logger == nil {
		return fmt.Errorf("create logger for %s/%d: logger factory returned nil", vm.Type, vm.Id)
	}
	vm.Logger = logger
	if !p.startManagedMonitor(&vm, true, true) {
		return fmt.Errorf("start monitor for %s/%d: service is stopping", vm.Type, vm.Id)
	}
	return nil
}

// check whether journalctl is available inside an LXC container
func (p *Pve) lxcHasJournalctl(strId string) bool {
	err := exec.Command("pct", "exec", strId, "--", "which", "journalctl").Run()
	return err == nil
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
func (p *Pve) CurrentLXCs() VMs {
	slog.Debug("updating list of running LXCs")
	vms := VMs{}
	out, err := exec.Command("pct", "list").Output()
	if err != nil {
		slog.Error(fmt.Sprintf("failure listing LXCs: %v", err))
		return vms
	}
	outStr := string(out)
	for _, line := range strings.Split(outStr, "\n") {
		items := strings.Fields(line)
		if len(items) < 3 {
			continue
		}
		strId := items[0]
		state := items[1]
		name := items[2]
		if state != "running" {
			continue
		}
		id, err := strconv.Atoi(strId)
		if err != nil {
			continue
		}
		if !p.checkLists(id) {
			continue
		}
		if !p.lxcHasJournalctl(strId) {
			slog.Debug(fmt.Sprintf("skipping lxc/%d: journalctl not found", id))
			continue
		}
		vms[id] = &VM{
			Id:         id,
			Name:       name,
			Type:       "lxc",
			MonitorCmd: "pct",
			MonitorArgs: []string{
				"exec",
				strId,
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
	return vms
}

// return a map containing the currently running KVMs
func (p *Pve) CurrentKVMs() VMs {
	slog.Debug("updating list of running KVMs")
	vms := VMs{}
	out, err := exec.Command("qm", "list").Output()
	if err != nil {
		slog.Error(fmt.Sprintf("failure listing KVMs: %v", err))
		return vms
	}
	outStr := string(out)
	for _, line := range strings.Split(outStr, "\n") {
		items := strings.Fields(line)
		if len(items) < 3 {
			continue
		}
		strId := items[0]
		name := items[1]
		state := items[2]
		if state != "running" {
			continue
		}
		id, err := strconv.Atoi(strId)
		if err != nil {
			continue
		}
		if !p.checkLists(id) {
			continue
		}
		vms[id] = &VM{
			Id:         id,
			Name:       name,
			Type:       "qm",
			MonitorCmd: "qm",
			MonitorArgs: []string{
				"exec",
				strId,
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
	return vms
}

// return a map containing the currently running LXCs and KVMs
func (p *Pve) CurrentVMs() VMs {
	vms := VMs{}
	if !p.cfg.SkipLXCs {
		maps.Copy(vms, p.CurrentLXCs())
	}
	/*
		// right now KVMs are not monitored, since the qm exec command
		// always block until the command exits, making it impossible to
		// parse the output as a stream.
		if !p.cfg.SkipKVMs {
			maps.Copy(vms, p.CurrentKVMs())
		}
	*/
	return vms
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

	// create logger without holding the map lock
	logger, err := p.newLogger(p.cfg, ologgers.OLoggerOptions{
		ServiceName: vm.Name,
		ServiceId:   fmt.Sprintf("%s/%d", vm.Type, vm.Id),
	})
	if err != nil {
		slog.Warn(fmt.Sprintf("unable to create a logger for %s/%d", vm.Type, vm.Id))
	}
	vm.Logger = logger

	// insert if still not present (double-checked locking)
	p.knownVMsMu.Lock()
	defer p.knownVMsMu.Unlock()
	if existing, ok := p.knownVMs[vm.Id]; ok {
		// someone else added it while we were creating logger
		return existing
	}
	p.knownVMs[vm.Id] = vm
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
	if stored.Logger != nil && p.startManagedMonitor(stored, false, false) {
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
	}

	p.knownVMsMu.Lock()
	if current, ok := p.knownVMs[id]; ok && current == vm {
		delete(p.knownVMs, id)
	}
	p.knownVMsMu.Unlock()
}

// refresh the map of running VMs
func (p *Pve) RefreshVMsMonitoring() {
	vms := p.CurrentVMs()
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
}

func (p *Pve) periodicRefresh() {
	p.RefreshVMsMonitoring()
	if p.cfg.RefreshInterval == 0 {
		return
	}
	p.ticker = time.NewTicker(time.Duration(p.cfg.RefreshInterval) * time.Second)
	quitTicker := make(chan bool)
	p.quitTicker = quitTicker
	go func() {
		for {
			select {
			case <-p.quitTicker:
				// was asked to stop
				return
			case <-p.ticker.C:
				// periodic task
				p.RefreshVMsMonitoring()
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
		p.quitTicker <- true
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
	}
	pending := make([]pendingStop, 0, len(ids)+1)
	if selfVM != nil {
		cancel, done := requestMonitorStop(selfVM, true)
		pending = append(pending, pendingStop{cancel: cancel, done: done})
	}
	for _, id := range ids {
		p.knownVMsMu.RLock()
		vm := p.knownVMs[id]
		p.knownVMsMu.RUnlock()
		if vm != nil {
			cancel, done := requestMonitorStop(vm, true)
			pending = append(pending, pendingStop{cancel: cancel, done: done})
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

	p.knownVMsMu.Lock()
	for _, id := range ids {
		delete(p.knownVMs, id)
	}
	p.knownVMsMu.Unlock()

	close(stopDone)
}
