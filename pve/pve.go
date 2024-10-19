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

type VMs map[int]*VM

type Pve struct {
	cfg        *config.Config
	knownVMs   VMs
	ticker     *time.Ticker
	quitTicker *chan bool
}

func New(cfg *config.Config) *Pve {
	pve := Pve{
		cfg:      cfg,
		knownVMs: VMs{},
	}
	return &pve
}

func (p *Pve) RunKeptAliveProcess(vm *VM) error {
	for round := 0; round < p.cfg.CmdRetryTimes; round++ {
		if round > 0 {
			time.Sleep(time.Duration(p.cfg.CmdRetryDelay) * time.Second)
		}
		if vm.MonitorCmd == "" {
			return errors.New("missing monitoring command")
		}
		finished := make(chan error, 1)
		ctx, cancel := context.WithCancel(context.Background())
		vm.StopProcess = cancel
		cmd := exec.CommandContext(ctx, vm.MonitorCmd, vm.MonitorArgs...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			slog.Error(fmt.Sprintf("failure opening standard output of vm/%d: %v", vm.Id, err))
			continue
		}
		err = cmd.Start()
		if err != nil {
			slog.Error(fmt.Sprintf("failed to run command: %v", err))
			continue
		}
		go func() {
			seenError := false
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				line := scanner.Text()
				var jData interface{}
				err := json.Unmarshal([]byte(line), &jData)
				if err != nil {
					if !seenError {
						slog.Error(fmt.Sprintf("failed parsing JSON for vm/%d; some logs will be sent as strings", vm.Id))
						seenError = true
					}
					vm.Logger.Log(line)
				} else {
					vm.Logger.Log(jData)
				}
			}
			err := cmd.Wait()
			if !vm.Running {
				err = nil
			}
			finished <- err
		}()
		err = <-finished
		if !vm.Running {
			break
		}
		if err != nil {
			vm.LastError = &err
		}
	}
	return nil
}

func (p *Pve) CurrentLXCs() VMs {
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

func (p *Pve) CurrentKVMs() VMs {
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

func (p *Pve) UpdateVM(l *VM) *VM {
	if _, ok := p.knownVMs[l.Id]; !ok {
		slog.Info("new, add VM")
		logger, err := ologgers.New(p.cfg, ologgers.OLoggerOptions{
			ServiceName: fmt.Sprintf("vm/%d", l.Id),
		})
		if err != nil {
			slog.Warn(fmt.Sprintf("unable to create a logger for vm/%d", l.Id))
		}
		l.Logger = logger
		p.knownVMs[l.Id] = l
	}
	return p.knownVMs[l.Id]
}

func (p *Pve) StartVMMonitoring(l *VM) {
	vm := p.UpdateVM(l)
	if vm.Logger != nil && !vm.Running {
		vm.Running = true
		go p.RunKeptAliveProcess(vm)
	}
}

func (p *Pve) StopVMMonitoring(id int) {
	if vm, ok := p.knownVMs[id]; ok {
		if vm.StopProcess != nil {
			vm.StopProcess()
		}
		vm.Running = false
	}
}

func (p *Pve) RemoveVM(id int) {
	slog.Info(fmt.Sprintf("remove VM %d", id))
	p.StopVMMonitoring(id)
	delete(p.knownVMs, id)
}

func (p *Pve) RefreshVMsMonitoring() {
	vms := p.CurrentVMs()
	for _, vm := range vms {
		p.StartVMMonitoring(vm)
	}

	remove := []int{}
	for id, vm := range p.knownVMs {
		if _, ok := vms[id]; !ok {
			slog.Info(fmt.Sprintf("Id %d (%s) vanished", id, vm.Name))
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
				return
			// interval task
			case <-p.ticker.C:
				p.RefreshVMsMonitoring()
			}
		}
	}()
}

func (p *Pve) Start() {
	if p.ticker != nil {
		return
	}
	p.periodicRefresh()
}

func (p *Pve) Stop() {
	p.ticker.Stop()
	*p.quitTicker <- true
	for id := range p.knownVMs {
		p.RemoveVM(id)
	}
}
