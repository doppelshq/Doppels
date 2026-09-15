package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPIDLockReclaimsStalePIDWhenDaemonStarts(t *testing.T) {
	configDir := t.TempDir()
	pidPath := filepath.Join(configDir, runnerPIDFile)
	if err := os.WriteFile(pidPath, []byte("2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithContext(ctx, filepath.Join(configDir, "runner.sock"), strings.Repeat("a", 64), "test", configDir)
	}()

	deadline := time.Now().Add(3 * time.Second)
	want := strconv.Itoa(os.Getpid()) + "\n"
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(pidPath)
		if err == nil && string(contents) == want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	contents, err := os.ReadFile(pidPath)
	if err != nil || string(contents) != want {
		cancel()
		<-done
		t.Fatalf("PID file = %q, %v; want %q", contents, err, want)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PID file remains after graceful shutdown: %v", err)
	}
}

func TestPIDLockRejectsLiveMatchingProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), runnerPIDFile)
	pid := os.Getpid()
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquirePIDLock(path, pid+1, func(got int) bool { return got == pid })
	if lock != nil {
		_ = lock.Close()
		t.Fatal("acquired PID lock owned by a live matching process")
	}
	want := "another instance already running with PID " + strconv.Itoa(pid)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestPIDLockOverwritesNonMatchingLivePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), runnerPIDFile)
	if err := os.WriteFile(path, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquirePIDLock(path, 42, func(int) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(contents), "42\n"; got != want {
		t.Fatalf("PID file = %q, want %q", got, want)
	}
}
