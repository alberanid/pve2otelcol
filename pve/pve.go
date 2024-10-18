package pve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/alberanid/pve2otelcol/config"
	"github.com/alberanid/pve2otelcol/ologgers"
)

type LXC struct {
	Id          int
	Name        string
	Running     bool
	Logger      *ologgers.OLogger
	StopProcess func()
}

type LXCs map[int]*LXC

type Pve struct {
	cfg        *config.Config
	knownLXCs  LXCs
	ticker     *time.Ticker
	quitTicker *chan bool
}

func New(cfg *config.Config) *Pve {
	pve := Pve{
		cfg:       cfg,
		knownLXCs: LXCs{},
	}
	return &pve
}

func (p *Pve) RunKeptAliveProcess(lxc *LXC) error {
	for round := 0; round < p.cfg.CmdRetryTimes; round++ {
		if round > 0 {
			time.Sleep(time.Duration(p.cfg.CmdRetryDelay) * time.Second)
		}
		finished := make(chan error, 1)
		ctx, cancel := context.WithCancel(context.Background())
		lxc.StopProcess = cancel
		cmdArgs := []string{
			"exec",
			fmt.Sprintf("%d", lxc.Id),
			"--",
			"journalctl",
			"--lines",
			"0",
			"--follow",
			"--output",
			"json",
		}
		cmd := exec.CommandContext(ctx, "pct", cmdArgs...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			slog.Error(fmt.Sprintf("failure opening standard output of lxc/%d: %v", lxc.Id, err))
			continue
		}
		err = cmd.Start()
		if err != nil {
			slog.Error(fmt.Sprintf("failed to run command: %v", err))
			continue
		}
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				line := scanner.Text()
				var jData interface{}
				err := json.Unmarshal([]byte(line), &jData)
				if err != nil {
					lxc.Logger.Log(line)
				} else {
					lxc.Logger.Log(jData)
				}
			}
			err := cmd.Wait()
			if !lxc.Running {
				err = nil
			}
			finished <- err
		}()
		<-finished
		if !lxc.Running {
			break
		}
	}
	return nil
}

func (p *Pve) CurrentLXCs() LXCs {
	lxcs := LXCs{}
	out, err := exec.Command("pct", "list").Output()
	if err != nil {
		slog.Error(fmt.Sprintf("failure listing LXCs: %v", err))
	}
	outStr := string(out)
	for _, line := range strings.Split(outStr, "\n") {
		items := strings.Fields(line)
		if len(items) < 3 {
			continue
		}
		id := items[0]
		state := items[1]
		name := items[2]
		if state != "running" {
			continue
		}
		numId, err := strconv.Atoi(id)
		if err != nil {
			continue
		}
		lxcs[numId] = &LXC{
			Id:   numId,
			Name: name,
		}
	}
	return lxcs
}

func (p *Pve) UpdateLXC(l *LXC) *LXC {
	if _, ok := p.knownLXCs[l.Id]; !ok {
		slog.Info("new, add LXC")
		logger, err := ologgers.New(p.cfg, ologgers.OLoggerOptions{
			ServiceName: fmt.Sprintf("lxc/%d", l.Id),
		})
		if err != nil {
			slog.Warn(fmt.Sprintf("unable to create a logger for lxc/%d", l.Id))
		}
		l.Logger = logger
		p.knownLXCs[l.Id] = l
	}
	return p.knownLXCs[l.Id]
}

func (p *Pve) StartLXCMonitoring(l *LXC) {
	lxc := p.UpdateLXC(l)
	if lxc.Logger != nil && !lxc.Running {
		lxc.Running = true
		go p.RunKeptAliveProcess(lxc)
	}
}

func (p *Pve) StopLXCMonitoring(id int) {
	if lxc, ok := p.knownLXCs[id]; ok {
		if lxc.StopProcess != nil {
			lxc.StopProcess()
		}
		lxc.Running = false
	}
}

func (p *Pve) RemoveLXC(id int) {
	slog.Info(fmt.Sprintf("remove LXC %d", id))
	p.StopLXCMonitoring(id)
	delete(p.knownLXCs, id)
}

func (p *Pve) RefreshLXCsMonitoring() {
	lxcs := p.CurrentLXCs()
	for _, lxc := range lxcs {
		p.StartLXCMonitoring(lxc)
	}

	remove := []int{}
	for id, lxc := range p.knownLXCs {
		if _, ok := lxcs[id]; !ok {
			slog.Info(fmt.Sprintf("Id %d (%s) vanished", id, lxc.Name))
			remove = append(remove, lxc.Id)
		}
	}
	for _, id := range remove {
		p.RemoveLXC(id)
	}
}

func (p *Pve) periodicRefresh() {
	p.ticker = time.NewTicker(time.Duration(p.cfg.RefreshInterval) * time.Second)
	quitTicker := make(chan bool)
	p.quitTicker = &quitTicker

	// Run the first refresh right now
	p.RefreshLXCsMonitoring()
	go func() {
		for {
			select {
			case <-*p.quitTicker:
				return
			// interval task
			case <-p.ticker.C:
				p.RefreshLXCsMonitoring()
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
	for id := range p.knownLXCs {
		p.RemoveLXC(id)
	}
}
