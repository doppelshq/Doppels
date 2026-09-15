package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const runnerPIDFile = "runner.pid"

// pidLock keeps an advisory lock open for the daemon lifetime. The decimal
// PID is deliberately human-readable; the kernel lock closes the startup
// race between two processes inspecting or rewriting the same file.
type pidLock struct {
	file *os.File
	path string
	info os.FileInfo
}

func acquirePIDLock(path string, pid int, matches func(int) bool) (*pidLock, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open PID file %s: %w", path, err)
	}
	closeFile := func() { _ = file.Close() }
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeFile()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			if owner, readErr := readPIDFile(path); readErr == nil {
				return nil, anotherInstanceError(owner)
			}
			return nil, fmt.Errorf("another instance already owns PID lock %s", path)
		}
		return nil, fmt.Errorf("lock PID file %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		closeFile()
		return nil, fmt.Errorf("secure PID file %s: %w", path, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		closeFile()
		return nil, fmt.Errorf("read PID file %s: %w", path, err)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		closeFile()
		return nil, fmt.Errorf("read PID file %s: %w", path, err)
	}
	if owner, parseErr := parsePID(contents); parseErr == nil && owner != pid && matches(owner) {
		closeFile()
		return nil, anotherInstanceError(owner)
	}
	if err := file.Truncate(0); err != nil {
		closeFile()
		return nil, fmt.Errorf("truncate PID file %s: %w", path, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		closeFile()
		return nil, fmt.Errorf("rewrite PID file %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", pid); err != nil {
		closeFile()
		return nil, fmt.Errorf("write PID file %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		closeFile()
		return nil, fmt.Errorf("sync PID file %s: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		closeFile()
		return nil, fmt.Errorf("stat PID file %s: %w", path, err)
	}
	return &pidLock{file: file, path: path, info: info}, nil
}

func (l *pidLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	var errs []error
	// Do not unlink a replacement file if an administrator deliberately
	// moved a new one into place while this daemon was alive.
	if info, err := os.Stat(l.path); err == nil && os.SameFile(info, l.info) {
		if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove PID file %s: %w", l.path, err))
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("stat PID file %s: %w", l.path, err))
	}
	if err := l.file.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close PID file %s: %w", l.path, err))
	}
	l.file = nil
	return errors.Join(errs...)
}

func readPIDFile(path string) (int, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parsePID(contents)
}

func parsePID(contents []byte) (int, error) {
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid PID %q", strings.TrimSpace(string(contents)))
	}
	return pid, nil
}

func anotherInstanceError(pid int) error {
	return fmt.Errorf("another instance already running with PID %d", pid)
}
