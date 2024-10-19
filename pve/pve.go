package pve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"strconv"
	"strings"
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
	Running     bool
	Logger      *ologgers.OLogger
	StopProcess func()
	LastError   *error
}

// map of VMID to VM information
type VMs map[int]*VM

// object used to interact with a Proxmox instance
type Pve struct {
	cfg        *config.Config
	knownVMs   VMs
	ticker     *time.Ticker
	quitTicker *chan bool
}

// return a Pve instance.
func New(cfg *config.Config) *Pve {
	pve := Pve{
		cfg:      cfg,
		knownVMs: VMs{},
	}
	return &pve
}

func (p *Pve) runVMMonitoring(vm *VM, ctx context.Context, finished chan error) {
	cmd := exec.CommandContext(ctx, vm.MonitorCmd, vm.MonitorArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Error(fmt.Sprintf("failure opening standard output of %s/%d: %v", vm.Type, vm.Id, err))
		finished <- err
	}
	err = cmd.Start()
	if err != nil {
		slog.Error(fmt.Sprintf("failure starting monitoring command of %s/%d: %v", vm.Type, vm.Id, err))
		finished <- err
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
	if !vm.Running {
		err = nil
	} else {
		slog.Error(fmt.Sprintf("failure running monitoring command of %s/%d: %v", vm.Type, vm.Id, err))
	}
	finished <- err
}

// run a command inside a VM and parse its output that will be sent to a OTLP collector
func (p *Pve) RunKeptAliveProcess(vm *VM) error {
	if vm.MonitorCmd == "" {
		return errors.New("missing monitoring command")
	}
	strCmd := fmt.Sprintf("%s %s", vm.MonitorCmd, strings.Join(vm.MonitorArgs, " "))
	slog.Debug(fmt.Sprintf("run monitoring process '%s'", strCmd))
	for round := 0; round < p.cfg.CmdRetryTimes; round++ {
		if round > 0 {
			// the process failed to run: try again after a delay
			slog.Warn(fmt.Sprintf("command '%s' failed; trying again in %d second(s) (run %d of %d)",
				strCmd, p.cfg.CmdRetryDelay, round, p.cfg.CmdRetryTimes))
			time.Sleep(time.Duration(p.cfg.CmdRetryDelay) * time.Second)
		}
		finished := make(chan error, 1)
		ctx, cancel := context.WithCancel(context.Background())
		// store the cancel function so that we can stop it from outside
		vm.StopProcess = cancel
		go p.runVMMonitoring(vm, ctx, finished)
		err := <-finished
		if !vm.Running {
			break
		}
		if err != nil {
			vm.LastError = &err
		}
	}
	return nil
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
	if _, ok := p.knownVMs[vm.Id]; !ok {
		slog.Debug(fmt.Sprintf("adding newly found VM %s/%d", vm.Type, vm.Id))
		logger, err := ologgers.New(p.cfg, ologgers.OLoggerOptions{
			ServiceName: fmt.Sprintf("%s/%d", vm.Type, vm.Id),
		})
		if err != nil {
			slog.Warn(fmt.Sprintf("unable to create a logger for %s/%d", vm.Type, vm.Id))
		}
		vm.Logger = logger
		// store the VM in the list of monitored VMs
		p.knownVMs[vm.Id] = vm
	}
	return vm
}

// run the monitoring process of a VM
func (p *Pve) StartVMMonitoring(vm *VM) {
	p.UpdateVM(vm)
	if vm.Logger != nil && !vm.Running {
		slog.Debug(fmt.Sprintf("start monitoring VM %s/%d", vm.Type, vm.Id))
		vm.Running = true
		go p.RunKeptAliveProcess(vm)
	}
}

// stop the monitoring process of a VM
func (p *Pve) StopVMMonitoring(id int) {
	if vm, ok := p.knownVMs[id]; ok {
		if vm.StopProcess != nil {
			slog.Debug(fmt.Sprintf("stop monitoring VM %s/%d", vm.Type, vm.Id))
			vm.StopProcess()
		}
		vm.Running = false
	}
}

// remove a VM from the list of known VMs
func (p *Pve) RemoveVM(id int) {
	vmDesc := fmt.Sprintf("%d", id)
	if vm, ok := p.knownVMs[id]; ok {
		vmDesc = fmt.Sprintf("%s/%d", vm.Type, id)
	}
	slog.Debug(fmt.Sprintf("remove VM %s", vmDesc))
	p.StopVMMonitoring(id)
	delete(p.knownVMs, id)
}

// refresh the map of running VMs
func (p *Pve) RefreshVMsMonitoring() {
	vms := p.CurrentVMs()
	for _, vm := range vms {
		p.StartVMMonitoring(vm)
	}

	remove := []int{}
	for id, vm := range p.knownVMs {
		if _, ok := vms[id]; !ok {
			remove = append(remove, vm.Id)
		}
	}
	for _, id := range remove {
		p.RemoveVM(id)
	}
}

func (p *Pve) periodicRefresh() {
	// Run the first refresh right now
	p.RefreshVMsMonitoring()
	if p.cfg.RefreshInterval == 0 {
		// no refresh: do not monitor for new/vanished VMs
		return
	}
	p.ticker = time.NewTicker(time.Duration(p.cfg.RefreshInterval) * time.Second)
	quitTicker := make(chan bool)
	p.quitTicker = &quitTicker
	go func() {
		for {
			select {
			case <-*p.quitTicker:
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
func (p *Pve) Start() {
	if p.ticker != nil {
		// do nothing, if already running
		return
	}
	slog.Info("start monitoring")
	p.periodicRefresh()
}

// stop all running monitoring processes
func (p *Pve) Stop() {
	slog.Info("stop monitoring")
	p.ticker.Stop()
	*p.quitTicker <- true
	for id := range p.knownVMs {
		p.RemoveVM(id)
	}
}
