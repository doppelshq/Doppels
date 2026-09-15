package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
