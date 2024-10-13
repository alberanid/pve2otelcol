package pve

import (
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

type Pve struct{}

func New() *Pve {
	pve := Pve{}
	return &pve
}

func (p *Pve) ListLXCs() []LXC {
	lxcs := []LXC{}
	out, err := exec.Command("pct", "list").Output()
	if err != nil {
		slog.Error("failure listing LXCs: %v", err)
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
		lxcs = append(lxcs, LXC{
			Id:      numId,
			Name:    name,
			Running: state == "running",
		})
	}
	return lxcs
}
