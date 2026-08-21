package pve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alberanid/pve2otelcol/ologgers"
)

type commandRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f commandRunnerFunc) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

type fakeProcess struct {
	stdout    io.ReadCloser
	stderr    io.Writer
	startErr  error
	waitErr   error
	cancelErr error
	started   bool
	waited    bool
}

func (p *fakeProcess) SetStderr(stderr io.Writer)         { p.stderr = stderr }
func (p *fakeProcess) StdoutPipe() (io.ReadCloser, error) { return p.stdout, nil }
func (p *fakeProcess) Start() error {
	p.started = true
	return p.startErr
}
func (p *fakeProcess) Wait() error {
	p.waited = true
	return p.waitErr
}
func (p *fakeProcess) Cancel() error { return p.cancelErr }

type fakeTimer struct {
	ch chan time.Time
}

func (t *fakeTimer) Chan() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop() bool             { return true }

type fakeTicker struct {
	ch chan time.Time
}

func (t *fakeTicker) Chan() <-chan time.Time { return t.ch }
func (t *fakeTicker) Stop()                  {}

type fakeClock struct {
	mu             sync.Mutex
	now            time.Time
	timerDurations []time.Duration
	timers         chan *fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0), timers: make(chan *fakeTimer, 10)}
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) NewTimer(d time.Duration) timer {
	t := &fakeTimer{ch: make(chan time.Time, 1)}
	c.mu.Lock()
	c.timerDurations = append(c.timerDurations, d)
	c.mu.Unlock()
	c.timers <- t
	return t
}
func (c *fakeClock) NewTicker(time.Duration) ticker {
	return &fakeTicker{ch: make(chan time.Time)}
}

func localResourceJSON(t *testing.T, id int, name, status string) string {
	t.Helper()
	node, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname error = %v", err)
	}
	return fmt.Sprintf(`[{"vmid":%d,"name":%q,"node":%q,"status":%q,"type":"lxc"}]`, id, name, node, status)
}

func TestCurrentLXCsUsesJSONDiscoveryAndCachesSuccessfulProbe(t *testing.T) {
	p, _, _ := newTestPve(t)
	p.cfg.SkipLXCs = false
	resourceJSON := localResourceJSON(t, 201, "name with spaces", "running")
	discoveryCalls := 0
	probeCalls := 0
	p.commands = commandRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Errorf("%s command context has no deadline", name)
		}
		switch name {
		case "pvesh":
			discoveryCalls++
			want := []string{"get", "/cluster/resources", "--type", "vm", "--output-format", "json"}
			if !reflect.DeepEqual(args, want) {
				t.Errorf("pvesh arguments = %q, want %q", args, want)
			}
			return []byte(resourceJSON), nil
		case "pct":
			probeCalls++
			return []byte("yes"), nil
		default:
			return nil, fmt.Errorf("unexpected command %q", name)
		}
	})

	for attempt := 0; attempt < 2; attempt++ {
		vms, err := p.CurrentLXCs()
		if err != nil {
			t.Fatalf("CurrentLXCs() attempt %d error = %v", attempt+1, err)
		}
		if vm := vms[sourceID("lxc", 201)]; vm == nil || vm.Name != "name with spaces" {
			t.Fatalf("CurrentLXCs() attempt %d VM = %#v", attempt+1, vm)
		}
	}
	if discoveryCalls != 2 || probeCalls != 1 {
		t.Fatalf("command calls = discovery:%d probe:%d, want 2/1", discoveryCalls, probeCalls)
	}

	resourceJSON = localResourceJSON(t, 201, "replacement", "running")
	if _, err := p.CurrentLXCs(); err != nil {
		t.Fatalf("CurrentLXCs() after identity change error = %v", err)
	}
	if probeCalls != 2 {
		t.Fatalf("probe calls after identity change = %d, want 2", probeCalls)
	}
}

func TestCurrentLXCsDoesNotCacheMissingCapability(t *testing.T) {
	p, _, _ := newTestPve(t)
	resourceJSON := localResourceJSON(t, 202, "without-journal", "running")
	probeCalls := 0
	p.commands = commandRunnerFunc(func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "pvesh" {
			return []byte(resourceJSON), nil
		}
		probeCalls++
		return []byte("no"), nil
	})
	for range 2 {
		vms, err := p.CurrentLXCs()
		if err != nil {
			t.Fatalf("CurrentLXCs() error = %v", err)
		}
		if len(vms) != 0 {
			t.Fatalf("CurrentLXCs() = %#v, want empty", vms)
		}
	}
	if probeCalls != 2 {
		t.Fatalf("probe calls = %d, want 2", probeCalls)
	}
}

func TestDiscoveryAndCapabilityCommandsHaveExplicitTimeouts(t *testing.T) {
	p, _, _ := newTestPve(t)
	p.cfg.DiscoveryTimeout = 2
	p.cfg.CapabilityProbeTimeout = 3
	p.commands = commandRunnerFunc(func(ctx context.Context, name string, _ ...string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("%s context has no deadline", name)
		}
		remaining := time.Until(deadline)
		want := 2 * time.Second
		if name == "pct" {
			want = 3 * time.Second
		}
		if remaining <= want-time.Second || remaining > want {
			t.Errorf("%s timeout remaining = %s, want approximately %s", name, remaining, want)
		}
		if name == "pvesh" {
			return []byte("[]"), nil
		}
		return []byte("yes"), nil
	})
	if _, err := p.currentResources(); err != nil {
		t.Fatalf("currentResources() error = %v", err)
	}
	if _, err := p.lxcHasJournalctl("203"); err != nil {
		t.Fatalf("lxcHasJournalctl() error = %v", err)
	}
}

func TestCurrentResourcesRejectsMalformedJSON(t *testing.T) {
	p, _, _ := newTestPve(t)
	p.commands = commandRunnerFunc(func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"vmid":`), nil
	})
	_, err := p.currentResources()
	if err == nil || !strings.Contains(err.Error(), "parse Proxmox resource JSON") {
		t.Fatalf("currentResources() error = %v, want JSON parse error", err)
	}
}

func TestRunVMMonitoringUsesInjectedProcessFactory(t *testing.T) {
	p, _, _ := newTestPve(t)
	p.cursorStore = newCursorStoreStub()
	p.logRecord = func(*ologgers.OLogger, interface{}) {}
	process := &fakeProcess{stdout: io.NopCloser(strings.NewReader("{\"MESSAGE\":\"hello\"}\n"))}
	var gotName string
	var gotArgs []string
	p.newProcess = func(_ context.Context, name string, args ...string) commandProcess {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return process
	}
	vm := &VM{Id: 204, Type: "lxc", MonitorCmd: "monitor", MonitorArgs: []string{"--json"}}
	if err := p.runVMMonitoring(context.Background(), vm); err != nil {
		t.Fatalf("runVMMonitoring() error = %v", err)
	}
	if gotName != "monitor" || !reflect.DeepEqual(gotArgs, []string{"--json"}) {
		t.Fatalf("process factory call = %q %q", gotName, gotArgs)
	}
	if !process.started || !process.waited || process.stderr == nil {
		t.Fatalf("process lifecycle = started:%t waited:%t stderr:%v", process.started, process.waited, process.stderr)
	}
}

func TestRetryDelayUsesInjectedClock(t *testing.T) {
	p, _, _ := newTestPve(t)
	p.cfg.CmdRetryTimes = 1
	p.cfg.CmdRetryDelay = 7
	clock := newFakeClock()
	p.clock = clock
	processErr := errors.New("failed")
	p.runProcess = func(context.Context, *VM) error { return processErr }
	done := make(chan error, 1)
	go func() {
		done <- p.RunKeptAliveProcess(context.Background(), &VM{Id: 205, Type: "lxc", MonitorCmd: "monitor"}, false)
	}()
	timer := <-clock.timers
	timer.ch <- clock.Now()
	if err := <-done; !errors.Is(err, processErr) {
		t.Fatalf("RunKeptAliveProcess() error = %v, want %v", err, processErr)
	}
	clock.mu.Lock()
	durations := append([]time.Duration(nil), clock.timerDurations...)
	clock.mu.Unlock()
	if !reflect.DeepEqual(durations, []time.Duration{7 * time.Second}) {
		t.Fatalf("timer durations = %v, want [7s]", durations)
	}
}
