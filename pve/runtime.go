package pve

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// commandRunner executes bounded, non-streaming commands. Keeping this behind
// an interface makes discovery and capability checks deterministic in tests.
type commandRunner interface {
	Output(context.Context, string, ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	configureCommandCancellation(cmd)
	return cmd.Output()
}

type commandProcess interface {
	SetStderr(io.Writer)
	StdoutPipe() (io.ReadCloser, error)
	Start() error
	Wait() error
	Cancel() error
}

type processFactory func(context.Context, string, ...string) commandProcess

type execProcess struct {
	cmd *exec.Cmd
}

func newExecProcess(ctx context.Context, name string, args ...string) commandProcess {
	cmd := exec.CommandContext(ctx, name, args...)
	configureCommandCancellation(cmd)
	return &execProcess{cmd: cmd}
}

func (p *execProcess) SetStderr(stderr io.Writer) {
	p.cmd.Stderr = stderr
}

func (p *execProcess) StdoutPipe() (io.ReadCloser, error) {
	return p.cmd.StdoutPipe()
}

func (p *execProcess) Start() error {
	return p.cmd.Start()
}

func (p *execProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *execProcess) Cancel() error {
	return p.cmd.Cancel()
}

func configureCommandCancellation(cmd *exec.Cmd) {
	// pct exec starts additional processes, including journalctl. Put the whole
	// tree in a dedicated process group so cancellation cannot leave descendants
	// holding stdout open and blocking Wait indefinitely.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = monitorCommandWaitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

type timer interface {
	Chan() <-chan time.Time
	Stop() bool
}

type ticker interface {
	Chan() <-chan time.Time
	Stop()
}

type clock interface {
	Now() time.Time
	NewTimer(time.Duration) timer
	NewTicker(time.Duration) ticker
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) timer { return realTimer{Timer: time.NewTimer(d)} }

func (realClock) NewTicker(d time.Duration) ticker { return realTicker{Ticker: time.NewTicker(d)} }

type realTimer struct{ *time.Timer }

func (t realTimer) Chan() <-chan time.Time { return t.C }

type realTicker struct{ *time.Ticker }

func (t realTicker) Chan() <-chan time.Time { return t.C }
