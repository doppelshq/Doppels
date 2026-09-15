package main

import (
	"context"
	"fmt"
	"time"
)

// gracefulShutdownTimeout bounds draining in-flight RPC handlers and local
// runs after SIGINT/SIGTERM. Supervisors get a deterministic upper bound.
const gracefulShutdownTimeout = 10 * time.Second

func serveWithGracefulShutdown(
	ctx context.Context,
	timeout time.Duration,
	serve func(context.Context) error,
	killProcessGroup func() error,
) error {
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		if err := killProcessGroup(); err != nil {
			return fmt.Errorf("graceful shutdown exceeded %s; SIGKILL process group: %w", timeout, err)
		}
		// Production cannot reach this return after a successful SIGKILL. It
		// keeps the kill seam deterministic for tests and unusual kernels.
		return fmt.Errorf("graceful shutdown exceeded %s; sent SIGKILL to process group", timeout)
	}
}
