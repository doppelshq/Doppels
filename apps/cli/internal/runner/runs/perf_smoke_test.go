//go:build perf

// Package runs' performance smoke test is opt-in: run it explicitly with
//
//	go test -tags perf ./internal/runner/runs/... -run TestPerfSmokeFiftyFastRuns -v
//
// It is intentionally excluded from every other `go test ./...` invocation
// (including this PR's own CI gate) because timing/RSS numbers are
// meaningful for a human watching a trend, not as a pass/fail gate that
// would otherwise flake on shared or loaded CI hardware.
package runs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPerfSmokeFiftyFastRuns starts 50 Runs of a fast shell recipe (this
// package's usual Capability/Recipe fixture machinery) with distinct
// idempotency keys, then reports wall time,
// peak RSS (Linux only, via /proc/self/status), and the net goroutine count
// versus a pre-run baseline. There is no pass/fail threshold: this is a
// smoke test for regressions a human reviews across runs, not a benchmark
// gate — a hard latency/memory assertion here would just be a flaky CI
// failure waiting to happen on shared hardware.
func TestPerfSmokeFiftyFastRuns(t *testing.T) {
	const fanout = 50
	service, root := runnerWorkspace(t, true)
	writeRunFixture(t, filepath.Join(root, ".doppels", "recipes", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: greet-shell, version: 1.0.0}
provides: [greet]
runtime: shell
defaults: {approval: never}
steps:
  - id: run
    name: Run
    run: {shell: sh, script: "export VALUE=ok"}
    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-perf", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	baselineGoroutines := runtime.NumGoroutine()
	baselineRSS := readRSSKiB(t)
	started := time.Now()

	runIDs := make([]string, fanout)
	for i := 0; i < fanout; i++ {
		key := fmt.Sprintf("perf-%d", i)
		params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"` + key + `"}`)
		result, rpcErr := manager.Start("cli", params)
		if rpcErr != nil {
			t.Fatalf("Start[%d]: %+v", i, rpcErr)
		}
		runIDs[i] = result.RunID
	}
	for i, runID := range runIDs {
		waitForStatus(t, root, runID, "succeeded")
		_ = i
	}
	elapsed := time.Since(started)

	deadline := time.Now().Add(5 * time.Second)
	var finalGoroutines int
	for {
		finalGoroutines = runtime.NumGoroutine()
		if finalGoroutines <= baselineGoroutines+10 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	peakRSS := readRSSKiB(t)

	t.Logf("perf smoke: %d Runs in %s (%.2f Runs/sec)", fanout, elapsed, float64(fanout)/elapsed.Seconds())
	t.Logf("perf smoke: goroutines baseline=%d final=%d (delta=%d)", baselineGoroutines, finalGoroutines, finalGoroutines-baselineGoroutines)
	if baselineRSS > 0 && peakRSS > 0 {
		t.Logf("perf smoke: RSS baseline=%d KiB peak=%d KiB (delta=%d KiB)", baselineRSS, peakRSS, peakRSS-baselineRSS)
	} else {
		t.Logf("perf smoke: RSS unavailable on this platform (Linux-only /proc/self/status)")
	}
}

// readRSSKiB reads this process's current resident set size from
// /proc/self/status; it returns 0 on any non-Linux platform or read error,
// since RSS reporting is a Linux-only diagnostic here, never a test
// precondition.
func readRSSKiB(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return value
	}
	return 0
}
