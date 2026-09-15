package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	shutdownHelperEnv     = "DOPPELS_SHUTDOWN_HELPER"
	shutdownHelperKill    = "kill-process-group"
	shutdownHelperFailure = "kill-process-group-fails"
	shutdownTestTimeout   = 200 * time.Millisecond
)

func TestGracefulShutdownTimeoutIsTenSeconds(t *testing.T) {
	if gracefulShutdownTimeout != 10*time.Second {
		t.Fatalf("graceful shutdown timeout = %s, want 10s", gracefulShutdownTimeout)
	}
}

func TestServeWithGracefulShutdownKillsSlowProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	var killed atomic.Bool
	serve := func(ctx context.Context) error {
		<-ctx.Done()
		<-release // model an in-flight handler that refuses to drain
		return nil
	}
	kill := func() error {
		killed.Store(true)
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- serveWithGracefulShutdown(ctx, 25*time.Millisecond, serve, kill)
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "graceful shutdown exceeded") {
			t.Fatalf("error = %v, want graceful timeout error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon wrapper did not exit after its graceful timeout")
	}
	if !killed.Load() {
		t.Fatal("slow daemon did not invoke the SIGKILL fallback")
	}
}

func TestServeWithGracefulShutdownDoesNotKillCleanExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var killed atomic.Bool
	serve := func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- serveWithGracefulShutdown(ctx, time.Second, serve, func() error {
			killed.Store(true)
			return nil
		})
	}()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if killed.Load() {
		t.Fatal("clean shutdown invoked SIGKILL fallback")
	}
}

func TestShutdownTimeoutKillsRealProcessGroup(t *testing.T) {
	cmd, stderr := startShutdownHelper(t, shutdownHelperKill)
	started := time.Now()
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to helper process group: %v", err)
	}
	err := waitForShutdownHelper(t, cmd)
	if err == nil {
		t.Fatal("runner helper exited successfully, want SIGKILL")
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("runner helper exit = %v, want process-group SIGKILL; stderr: %s", err, stderr)
	}
	if elapsed := time.Since(started); elapsed < shutdownTestTimeout-50*time.Millisecond || elapsed > shutdownTestTimeout+time.Second {
		t.Fatalf("runner helper took %s to exit, want approximately %s", elapsed, shutdownTestTimeout)
	}
}

func TestShutdownTimeoutReportsProcessGroupKillFailure(t *testing.T) {
	cmd, stderr := startShutdownHelper(t, shutdownHelperFailure)
	started := time.Now()
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to helper process group: %v", err)
	}
	if err := waitForShutdownHelper(t, cmd); err == nil {
		t.Fatal("runner helper exited successfully, want kill failure")
	}
	if elapsed := time.Since(started); elapsed < shutdownTestTimeout-50*time.Millisecond || elapsed > shutdownTestTimeout+time.Second {
		t.Fatalf("runner helper took %s to exit, want approximately %s", elapsed, shutdownTestTimeout)
	}
	if got := stderr.String(); !strings.Contains(got, "SIGKILL process group: injected kill failure") {
		t.Fatalf("runner helper error is unclear: %q", got)
	}
}

func startShutdownHelper(t *testing.T, mode string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestShutdownSubprocessHelper$")
	cmd.Env = append(os.Environ(), shutdownHelperEnv+"="+mode)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := bufio.NewScanner(stdout)
	readyLine := make(chan string, 1)
	go func() {
		if ready.Scan() {
			readyLine <- ready.Text()
			return
		}
		readyLine <- ""
	}()
	select {
	case line := <-readyLine:
		if line == "READY" {
			return cmd, stderr
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatalf("runner helper did not become ready: stdout=%q stderr=%q", line, stderr)
	case <-time.After(time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatalf("runner helper did not become ready within 1s: stderr=%q", stderr)
	}
	return nil, nil
}

func waitForShutdownHelper(t *testing.T, cmd *exec.Cmd) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(shutdownTestTimeout + time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatal("runner helper hung after its graceful shutdown timeout")
		return nil
	}
}

func TestShutdownSubprocessHelper(t *testing.T) {
	mode := os.Getenv(shutdownHelperEnv)
	if mode == "" {
		return
	}
	if err := prepareProcessGroup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()

	if mode == shutdownHelperKill {
		child := exec.Command("sh", "-c", `trap '' TERM; echo CHILD_READY; while :; do sleep 60; done`)
		childOutput, err := child.StdoutPipe()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		childReady := bufio.NewScanner(childOutput)
		if !childReady.Scan() || childReady.Text() != "CHILD_READY" {
			fmt.Fprintln(os.Stderr, "fake subprocess did not become ready")
			os.Exit(2)
		}
	}

	fmt.Println("READY")
	serve := func(context.Context) error { select {} }
	kill := killOwnProcessGroup
	if mode == shutdownHelperFailure {
		kill = func() error { return errors.New("injected kill failure") }
	}
	err := serveWithGracefulShutdown(ctx, shutdownTestTimeout, serve, kill)
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
