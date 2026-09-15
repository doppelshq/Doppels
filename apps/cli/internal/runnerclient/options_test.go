package runnerclient_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"doppels.so/cli/internal/runnerclient"
)

// TestDialMissingTokenFileReturnsErrNotRunning covers the default (no
// explicit Options.Token) path: cmd/doppels-runner only ever writes
// runner.token once it has started, so a config dir with a socket path but
// no token file yet is the same "no daemon here" condition as a missing
// socket, not a distinct filesystem error a caller must special-case.
func TestDialMissingTokenFileReturnsErrNotRunning(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("DOPPELS_RUNNER_CONFIG", configDir)

	_, err := runnerclient.Dial(context.Background(), runnerclient.Options{
		SocketPath: filepath.Join(configDir, "runner.sock"),
	})
	if !errors.Is(err, runnerclient.ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}
