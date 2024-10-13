package pve

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
)

type LXC struct {
	Id      int
	Name    string
	Running bool
}

type LXCs map[int]LXC

var KnownLXCs = LXCs{}

type Pve struct{}

func New() *Pve {
	pve := Pve{}
	return &pve
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
		lxcs[numId] = LXC{
			Id:      numId,
			Name:    name,
			Running: state == "running",
		}
	}
	return lxcs
}

func (p *Pve) RefreshLXCsMonitoring() {
	lxcs := p.CurrentLXCs()
	for id, lxc := range lxcs {
		if knownLXC, ok := KnownLXCs[id]; ok {
			slog.Info(fmt.Sprintf("Id %d (%s, running:%t known-running:%t) already known",
				id, lxc.Name, lxc.Running, knownLXC.Running))
			if lxc.Running && knownLXC.Running {
				slog.Info("already monitored, do nothing")
			} else if lxc.Running && !knownLXC.Running {
				slog.Info("from stopped to running: start monitoring")
			} else if !lxc.Running && knownLXC.Running {
				slog.Info("from running to stopped: stop monitoring")
			}
		} else {
			slog.Info(fmt.Sprintf("Id %d (%s, running:%t) not already known", id, lxc.Name, lxc.Running))
			if lxc.Running {
				slog.Info("new and running, start monitoring")
			} else {
				slog.Info("new but stopped, add but do not monitor")
			}
		}
	}

	remove := []int{}
	for id, lxc := range KnownLXCs {
		if _, ok := lxcs[id]; !ok {
			slog.Info(fmt.Sprintf("Id %d (%s) vanished", id, lxc.Name))
		}
		if lxc.Running {
			slog.Info("stop monitoring")
		}
		slog.Info("remove from KnownLXCs")
		remove = append(remove, id)
	}
	for _, id := range remove {
		delete(KnownLXCs, id)
	}
}
