package pve

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/alberanid/pve2otelcol/ologgers"
)

type cursorSave struct {
	source string
	cursor string
}

type cursorStoreStub struct {
	mu      sync.Mutex
	cursors map[string]string
	loads   []string
	saves   []cursorSave
}

func newCursorStoreStub() *cursorStoreStub {
	return &cursorStoreStub{cursors: make(map[string]string)}
}

func (s *cursorStoreStub) Load(source string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads = append(s.loads, source)
	return s.cursors[source], nil
}

func (s *cursorStoreStub) Save(source, cursor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, cursorSave{source: source, cursor: cursor})
	s.cursors[source] = cursor
	return nil
}

func (s *cursorStoreStub) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saves)
}

func (s *cursorStoreStub) snapshot() ([]string, []cursorSave) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.loads...), append([]cursorSave(nil), s.saves...)
}

func TestMonitorArgsWithCursorReplacesTailOption(t *testing.T) {
	vm := &VM{
		MonitorArgs: []string{"exec", "101", "--", "journalctl", "--lines", "0", "--follow", "--output", "json"},
		cursor:      "cursor-101",
	}

	got := monitorArgsWithCursor(vm)
	want := []string{"exec", "101", "--", "journalctl", "--follow", "--output", "json", "--after-cursor", "cursor-101", "--no-tail"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("monitorArgsWithCursor() = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(vm.MonitorArgs, []string{"exec", "101", "--", "journalctl", "--lines", "0", "--follow", "--output", "json"}) {
		t.Fatal("monitorArgsWithCursor mutated the base monitor arguments")
	}
}

func TestRunVMMonitoringResumesAfterLastLoggedCursor(t *testing.T) {
	p, _, _ := newTestPve(t)
	store := newCursorStoreStub()
	p.cursorStore = store
	logged := 0
	p.logRecord = func(_ *ologgers.OLogger, _ interface{}) {
		logged++
		if logged == 1 && store.saveCount() != 0 {
			t.Error("cursor advanced before the first record was handed to the logger")
		}
	}

	argsPath := filepath.Join(t.TempDir(), "arguments")
	script := `args_file=$1; shift
printf 'start\n' >> "$args_file"
printf '%s\n' "$@" >> "$args_file"
printf 'end\n' >> "$args_file"
printf '{"__CURSOR":"cursor-1","MESSAGE":"entry"}\n'`
	vm := &VM{
		Id:         130,
		Type:       "lxc",
		MonitorCmd: "/bin/sh",
		MonitorArgs: []string{
			"-c", script, "monitor", argsPath, "--lines", "0", "--follow",
		},
	}

	if err := p.runVMMonitoring(context.Background(), vm); err != nil {
		t.Fatalf("first runVMMonitoring() error = %v", err)
	}
	if err := p.runVMMonitoring(context.Background(), vm); err != nil {
		t.Fatalf("second runVMMonitoring() error = %v", err)
	}

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", argsPath, err)
	}
	wantArgs := strings.Join([]string{
		"start",
		"--lines",
		"0",
		"--follow",
		"end",
		"start",
		"--follow",
		"--after-cursor",
		"cursor-1",
		"--no-tail",
		"end",
		"",
	}, "\n")
	if got := string(data); got != wantArgs {
		t.Fatalf("monitor invocation arguments:\n%s\nwant:\n%s", got, wantArgs)
	}
	loads, saves := store.snapshot()
	if !reflect.DeepEqual(loads, []string{"lxc-130"}) {
		t.Fatalf("cursor loads = %#v, want lxc-130 once", loads)
	}
	if !reflect.DeepEqual(saves, []cursorSave{{source: "lxc-130", cursor: "cursor-1"}}) {
		t.Fatalf("cursor saves = %#v, want cursor-1 once", saves)
	}
	if logged != 2 {
		t.Fatalf("logged records = %d, want 2", logged)
	}
}

func TestFileCursorStorePersistsAcrossInstances(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cursors")
	first := &fileCursorStore{dir: dir}
	if err := first.Save("pve-0", "cursor-persisted"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	second := &fileCursorStore{dir: dir}
	got, err := second.Load("pve-0")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got != "cursor-persisted" {
		t.Fatalf("Load() = %q, want cursor-persisted", got)
	}
	info, err := os.Stat(filepath.Join(dir, "pve-0.cursor"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Fatalf("cursor file mode = %o, want 600", gotMode)
	}
}

func TestRunVMMonitoringLoadsPersistedCursor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cursors")
	store := &fileCursorStore{dir: dir}
	if err := store.Save("pve-0", "cursor-from-disk"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	p, _, _ := newTestPve(t)
	p.cursorStore = &fileCursorStore{dir: dir}
	p.logRecord = func(_ *ologgers.OLogger, _ interface{}) {}
	argsPath := filepath.Join(t.TempDir(), "arguments")
	script := `args_file=$1; shift
printf '%s\n' "$@" > "$args_file"
printf '{"MESSAGE":"entry without a newer cursor"}\n'`
	vm := &VM{
		Id:         0,
		Type:       "pve",
		MonitorCmd: "/bin/sh",
		MonitorArgs: []string{
			"-c", script, "monitor", argsPath, "--lines", "0", "--follow",
		},
	}
	if err := p.runVMMonitoring(context.Background(), vm); err != nil {
		t.Fatalf("runVMMonitoring() error = %v", err)
	}

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", argsPath, err)
	}
	want := "--follow\n--after-cursor\ncursor-from-disk\n--no-tail\n"
	if got := string(data); got != want {
		t.Fatalf("monitor arguments = %q, want %q", got, want)
	}
}

func TestFileCursorStoreRejectsUnsafeSourceAndCursor(t *testing.T) {
	store := &fileCursorStore{dir: t.TempDir()}
	if err := store.Save("../outside", "cursor"); err == nil {
		t.Fatal("Save() accepted an unsafe source name")
	}
	if err := store.Save("lxc-101", "cursor\nsecond-line"); err == nil {
		t.Fatal("Save() accepted a cursor containing a newline")
	}
}
