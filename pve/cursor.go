package pve

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const cursorPersistInterval = time.Second
const maxCursorFileSize = 4096

type cursorStore interface {
	Load(source string) (string, error)
	Save(source, cursor string) error
}

type disabledCursorStore struct{}

func (disabledCursorStore) Load(string) (string, error) {
	return "", nil
}

func (disabledCursorStore) Save(string, string) error {
	return nil
}

type fileCursorStore struct {
	dir string
}

func newCursorStore(dir string) cursorStore {
	if dir == "" {
		return disabledCursorStore{}
	}
	return &fileCursorStore{dir: dir}
}

func (s *fileCursorStore) Load(source string) (string, error) {
	if err := validateCursorSource(source); err != nil {
		return "", err
	}
	path := filepath.Join(s.dir, source+".cursor")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxCursorFileSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxCursorFileSize {
		return "", fmt.Errorf("cursor file exceeds %d bytes", maxCursorFileSize)
	}
	cursor := strings.TrimSpace(string(data))
	if err := validateCursor(cursor); err != nil {
		return "", err
	}
	return cursor, nil
}

func (s *fileCursorStore) Save(source, cursor string) (err error) {
	if err := validateCursorSource(source); err != nil {
		return err
	}
	if err := validateCursor(cursor); err != nil {
		return err
	}
	if cursor == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return err
	}

	temp, err := os.CreateTemp(s.dir, ".cursor-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err = temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err = io.WriteString(temp, cursor+"\n"); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tempPath, filepath.Join(s.dir, source+".cursor")); err != nil {
		return err
	}

	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func validateCursorSource(source string) error {
	if source == "" || filepath.Base(source) != source || strings.ContainsAny(source, `/\\`) {
		return fmt.Errorf("invalid cursor source %q", source)
	}
	return nil
}

func validateCursor(cursor string) error {
	if len(cursor) > maxCursorFileSize {
		return fmt.Errorf("cursor exceeds %d bytes", maxCursorFileSize)
	}
	if strings.ContainsAny(cursor, "\r\n\x00") {
		return errors.New("cursor contains an invalid control character")
	}
	return nil
}

func cursorSource(vm *VM) (string, error) {
	if vm.Id < 0 {
		return "", fmt.Errorf("invalid negative source ID %d", vm.Id)
	}
	for _, r := range vm.Type {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return "", fmt.Errorf("invalid source type %q", vm.Type)
		}
	}
	if vm.Type == "" {
		return "", errors.New("missing source type")
	}
	return fmt.Sprintf("%s-%d", vm.Type, vm.Id), nil
}

func (p *Pve) loadCursor(vm *VM) error {
	vm.cursorMu.Lock()
	if vm.cursorLoaded {
		vm.cursorMu.Unlock()
		return nil
	}
	vm.cursorMu.Unlock()

	source, err := cursorSource(vm)
	if err != nil {
		return err
	}
	cursor, err := p.cursorStore.Load(source)
	if err != nil {
		return fmt.Errorf("load cursor for %s/%d: %w", vm.Type, vm.Id, err)
	}
	if err := validateCursor(cursor); err != nil {
		return fmt.Errorf("load cursor for %s/%d: %w", vm.Type, vm.Id, err)
	}

	vm.cursorMu.Lock()
	if !vm.cursorLoaded {
		vm.cursor = cursor
		vm.persistedCursor = cursor
		vm.cursorLoaded = true
	}
	vm.cursorMu.Unlock()
	return nil
}

func currentCursor(vm *VM) string {
	vm.cursorMu.Lock()
	defer vm.cursorMu.Unlock()
	return vm.cursor
}

func (p *Pve) advanceCursor(vm *VM, record interface{}) {
	data, ok := record.(map[string]interface{})
	if !ok {
		return
	}
	cursor, ok := data["__CURSOR"].(string)
	if !ok || cursor == "" {
		return
	}
	if err := validateCursor(cursor); err != nil {
		slog.Warn("ignoring invalid journal cursor", "source", vm.sourceID().String(), "error", err)
		return
	}

	vm.cursorMu.Lock()
	vm.cursor = cursor
	vm.cursorMu.Unlock()
	p.persistCursor(vm, false)
}

func (p *Pve) persistCursor(vm *VM, force bool) {
	now := p.clock.Now()
	vm.cursorMu.Lock()
	if vm.cursor == "" || vm.cursor == vm.persistedCursor {
		vm.cursorMu.Unlock()
		return
	}
	if !force && !vm.lastCursorPersistAttempt.IsZero() && now.Sub(vm.lastCursorPersistAttempt) < cursorPersistInterval {
		vm.cursorMu.Unlock()
		return
	}
	cursor := vm.cursor
	vm.lastCursorPersistAttempt = now
	vm.cursorMu.Unlock()

	source, err := cursorSource(vm)
	if err == nil {
		err = p.cursorStore.Save(source, cursor)
	}
	if err != nil {
		slog.Error("unable to persist journal cursor", "source", vm.sourceID().String(), "error", err)
		return
	}

	vm.cursorMu.Lock()
	if vm.persistedCursor != cursor {
		vm.persistedCursor = cursor
	}
	vm.cursorMu.Unlock()
}

func monitorArgsWithCursor(vm *VM) []string {
	args := append([]string(nil), vm.MonitorArgs...)
	cursor := currentCursor(vm)
	if cursor == "" {
		return args
	}

	filtered := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--lines":
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(args[i], "--lines="):
		case args[i] == "--after-cursor":
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(args[i], "--after-cursor="):
		case args[i] == "--no-tail":
		default:
			filtered = append(filtered, args[i])
		}
	}
	return append(filtered, "--after-cursor", cursor, "--no-tail")
}
