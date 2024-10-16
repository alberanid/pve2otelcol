package pve

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/alberanid/pve2otelcol/config"
	"github.com/alberanid/pve2otelcol/ologgers"
	"golang.org/x/exp/rand"
)

type LXC struct {
	Id          int
	Name        string
	Running     bool
	Logger      *ologgers.OLogger
	StopProcess func()
}

type LXCs map[int]*LXC

var KnownLXCs = LXCs{}

type Pve struct {
	cfg                *config.Config
	UpdateIntervalSecs int
	ticker             *time.Ticker
	quitTicker         *chan bool
}

func New(cfg *config.Config) *Pve {
	pve := Pve{
		cfg:                cfg,
		UpdateIntervalSecs: 10,
	}
	pve.periodicRefresh()
	return &pve
}

func runMonitoringProcess(id int) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	cmdArgs := []string{
		"exec",
		fmt.Sprintf("%d", id),
		"--",
		"journalctl",
		"-f",
		"-o",
		"json",
	}
	cmd := exec.CommandContext(ctx, "pct", cmdArgs...)
	stdout, _ := cmd.StdoutPipe()
	cmd.Start()
	go func() {
		defer cmd.Wait()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			s := scanner.Text()
			fmt.Println(s)
		}
	}()
	return cancel
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
		numId, err := strconv.Atoi(id)
		if err != nil {
			continue
		}
		lxcs[numId] = &LXC{
			Id:      numId,
			Name:    name,
			Running: state == "running",
		}
	}
	return lxcs
}
func (p *Pve) GetKnownLXC(l *LXC) *LXC {
	if knownLXC, ok := KnownLXCs[l.Id]; ok {
		return knownLXC
	}
	return nil
}

func (p *Pve) AddLXC(l *LXC) *LXC {
	if _, ok := KnownLXCs[l.Id]; !ok {
		slog.Info("new, add LXC")
		logger, err := ologgers.New(ologgers.OLoggerOptions{
			Endpoint:    p.cfg.OtlpgRPCURL,
			ServiceName: fmt.Sprintf("lxc/%d", l.Id),
		})
		if err != nil {
			slog.Warn(fmt.Sprintf("unable to create a logger for lxc/%d", l.Id))
		}
		l.Logger = logger
		l.StopProcess = runMonitoringProcess(l.Id)
		KnownLXCs[l.Id] = l
	}
	return KnownLXCs[l.Id]
}

func (p *Pve) StartLXCMonitoring(l *LXC) {
	slog.Info("from stopped to running: start monitoring")
	lxc := p.AddLXC(l)
	lxc.Running = true
}

func (p *Pve) StopLXCMonitoring(l *LXC) {
	slog.Info("from running to stopped: stop monitoring")
	lxc := p.AddLXC(l)
	if l.StopProcess != nil {
		l.StopProcess()
	}
	lxc.Running = false
}

func (p *Pve) RemoveLXC(l *LXC) {
	slog.Info(fmt.Sprintf("remove LXC %d", l.Id))
	p.StopLXCMonitoring(l)
	delete(KnownLXCs, l.Id)
}

func (p *Pve) RefreshLXCsMonitoring() {
	lxcs := p.CurrentLXCs()
	for id, lxc := range lxcs {
		if knownLXC, ok := KnownLXCs[id]; ok {
			slog.Debug(fmt.Sprintf("Id %d (%s, running:%t known-running:%t) already known",
				id, lxc.Name, lxc.Running, knownLXC.Running))
			if lxc.Running && !knownLXC.Running {
				p.StartLXCMonitoring(lxc)
			} else if !lxc.Running && knownLXC.Running {
				p.StopLXCMonitoring(lxc)
			}
		} else {
			slog.Info(fmt.Sprintf("Id %d (%s, running:%t) not already known", id, lxc.Name, lxc.Running))
			if lxc.Running {
				p.StartLXCMonitoring(lxc)
			} else {
				p.AddLXC(lxc)
			}
		}
	}

	remove := []*LXC{}
	for id, lxc := range KnownLXCs {
		if _, ok := lxcs[id]; !ok {
			slog.Info(fmt.Sprintf("Id %d (%s) vanished", id, lxc.Name))
			if lxc.Running {
				slog.Info("stop monitoring")
			}
			slog.Info("remove from KnownLXCs")
			remove = append(remove, lxc)
		}
	}
	for _, lxc := range remove {
		p.RemoveLXC(lxc)
	}

	// XXX: test, remove
	for id, lxc := range KnownLXCs {
		rnd := rand.New(rand.NewSource(uint64(time.Now().UnixNano()))).Uint32()
		lxc.Logger.Log(fmt.Sprintf("id:%d rnd:%d", id, rnd))
	}
}

func (p *Pve) periodicRefresh() {
	p.ticker = time.NewTicker(time.Duration(p.UpdateIntervalSecs) * time.Second)
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

func (p *Pve) Stop() {
	p.ticker.Stop()
	*p.quitTicker <- true
	for id, lxc := range KnownLXCs {
		if !lxc.Running {
			continue
		}
		slog.Info(fmt.Sprintf("stop monitoring lxc/%d (%s)", id, lxc.Name))
	}
}
