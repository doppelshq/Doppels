package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/runs"
	"doppels.so/cli/internal/runner/transport"
	"doppels.so/cli/internal/runner/workspace"
)

// SIGKILL delivery to a specific PID (as opposed to POSIX signal semantics
// generally) is a Unix concept; Windows has no equivalent, and the
// Runner's own graceful-shutdown fallback is POSIX-only too (see
// cmd/doppels-runner/processgroup_unix.go). These tests build and exec the
// real doppels-runner binary and kill -9 it, so they only make sense here.
func skipUnlessUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL recovery is a POSIX daemon-lifecycle concern; not applicable on Windows")
	}
}

// runnerBinary builds the real cmd/doppels-runner binary once per test
// process and reuses it: these tests need the actual daemon (PID file
// locking, socket reclaim, SIGINT/SIGTERM/SIGKILL handling), not the
// in-process server.Server harness the rest of this package uses.
var runnerBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "doppels-runner-conformance-bin")
	if err != nil {
		return "", err
	}
	binaryPath := filepath.Join(dir, "doppels-runner")
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolve conformance package source path")
	}
	// thisFile is .../apps/cli/internal/runner/conformance/sigkill_recovery_test.go;
	// the module root (go.mod, cmd/doppels-runner) is three directories up.
	cliRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	cmd := exec.Command("go", "build", "-o", binaryPath, "doppels.so/cli/cmd/doppels-runner")
	cmd.Dir = cliRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build doppels-runner: %w: %s", err, output)
	}
	return binaryPath, nil
})

const sigkillTestToken = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// sigkillDaemon manages one launch of the real doppels-runner subprocess
// against a fixed configDir/socketPath, so a test can kill -9 it and relaunch
// a fresh instance pointed at exactly the same on-disk state — the scenario
// this whole file exists to exercise.
type sigkillDaemon struct {
	t          *testing.T
	binary     string
	configDir  string
	socketPath string
	env        []string

	cmd *exec.Cmd
}

func newSigkillDaemon(t *testing.T, configDir, socketPath string, env []string) *sigkillDaemon {
	t.Helper()
	binary, err := runnerBinary()
	if err != nil {
		t.Fatalf("build doppels-runner: %v", err)
	}
	return &sigkillDaemon{t: t, binary: binary, configDir: configDir, socketPath: socketPath, env: env}
}

// start launches a fresh doppels-runner process and waits for it to accept
// and correctly answer v1/initialize on socketPath, or fails the test.
func (d *sigkillDaemon) start() {
	d.t.Helper()
	cmd := exec.Command(d.binary,
		"-config", d.configDir,
		"-socket", d.socketPath,
		"-token", sigkillTestToken,
		"-runner-version", "0.0.1-sigkill-test",
	)
	cmd.Env = d.env
	cmd.Stdout = &prefixedWriter{t: d.t, prefix: "[runner stdout] "}
	cmd.Stderr = &prefixedWriter{t: d.t, prefix: "[runner stderr] "}
	if err := cmd.Start(); err != nil {
		d.t.Fatalf("start doppels-runner: %v", err)
	}
	d.cmd = cmd

	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if exited := cmd.ProcessState; exited != nil {
			d.t.Fatalf("doppels-runner exited early during startup: %v", exited)
		}
		if err := d.probe(); err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		return
	}
	d.t.Fatalf("doppels-runner never became reachable at %s: %v", d.socketPath, lastErr)
}

func (d *sigkillDaemon) probe() error {
	conn, err := transport.Unix{}.Dial(d.socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := proto.NewEncoder(conn).WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": "probe", "method": "v1/initialize",
		"params": map[string]any{"token": sigkillTestToken, "client": map[string]any{"name": "conformance", "version": "0.0.1"}},
	}); err != nil {
		return err
	}
	frame, err := proto.NewDecoder(conn).ReadFrame()
	if err != nil {
		return err
	}
	var response proto.Response
	if err := json.Unmarshal(frame, &response); err != nil {
		return err
	}
	if response.Err != nil {
		return fmt.Errorf("probe initialize: %+v", response.Err)
	}
	return nil
}

// sigkill sends SIGKILL to the daemon process and reaps it: an uncatchable
// signal, exactly the "the machine lost power to this one process" scenario
// PR10 is meant to make the next launch recover from cleanly.
func (d *sigkillDaemon) sigkill() {
	d.t.Helper()
	if d.cmd == nil || d.cmd.Process == nil {
		d.t.Fatal("sigkill: daemon was never started")
	}
	if err := d.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		d.t.Fatalf("SIGKILL doppels-runner: %v", err)
	}
	_ = d.cmd.Wait()
	d.cmd = nil
}

func (d *sigkillDaemon) stop() {
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = d.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = d.cmd.Process.Kill()
		<-done
	}
	d.cmd = nil
}

// prefixedWriter forwards a subprocess's output into t.Log, so a failing
// test shows the daemon's own diagnostics instead of a bare timeout.
type prefixedWriter struct {
	t      *testing.T
	prefix string
}

func (w *prefixedWriter) Write(data []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line != "" {
			w.t.Log(w.prefix + line)
		}
	}
	return len(data), nil
}

// seedSigkillWorkspace lays out a workspace with a Capability and a Recipe
// whose Step writes its own PID to $TMPDIR/step.pid before sleeping (TMPDIR
// is one of the fixed handful of host env vars execution/runner.go's
// minimalEnvironment allowlists into every Step's sandboxed environment; an
// arbitrary custom var is silently stripped) — this file's only way to
// observe, from the outside, whether the Step subprocess an already-dead
// Runner started is still alive — then pre-registers the workspace into a
// fresh configDir's workspaces.json exactly as `doppels workspace add` would,
// so the daemon subprocess discovers it on its very first startup.
func seedSigkillWorkspace(t *testing.T, configDir string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspace")
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, ".doppels", "capabilities", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: greet, version: 1.0.0}
inputs:
  count: {type: integer, required: true}
outputs:
  value: {type: string}
`)
	writeFixture(t, filepath.Join(root, ".doppels", "recipes", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: greet-shell, version: 1.0.0}
provides: [greet]
runtime: shell
defaults: {approval: never}
steps:
  - id: run
    name: Run
    run: {shell: sh, script: "echo $$ > \"$TMPDIR/step.pid\"; sleep 20; export VALUE=ok"}
    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)
	registry := workspace.NewRegistry(filepath.Join(configDir, "workspaces.json"))
	if err := registry.Load(); err != nil {
		t.Fatal(err)
	}
	service := workspace.NewService(registry, workspace.Deps{Host: manifest.OSHost{}})
	if _, _, err := service.AddWorkspace(root); err != nil {
		t.Fatal(err)
	}
	return root
}

// dialSigkillClient completes the v1 handshake against a running daemon.
func dialSigkillClient(t *testing.T, socketPath string) *rawClient {
	t.Helper()
	conn, err := transport.Unix{}.Dial(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	client := &rawClient{t: t, conn: conn, encoder: proto.NewEncoder(conn), decoder: proto.NewDecoder(conn)}
	response := client.call("init", "v1/initialize", map[string]any{
		"token":  sigkillTestToken,
		"client": map[string]any{"name": "conformance", "version": "0.0.1"},
	})
	if response.Err != nil {
		t.Fatalf("initialize against daemon: %+v", response.Err)
	}
	return client
}

// processAlive reports whether pid still names a live process (any process,
// including one this test no longer owns — that's the point: after the
// Runner is killed, the Step subprocess it started is reparented to init and
// this test has no other handle on it).
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestSIGKILLRunnerLevelRecoverySocketAndPIDFile reproduces the desktop-first
// plan's "Runner-level kill" scenario: a live daemon that has already
// accepted and started a Run gets SIGKILL'd — the uncatchable signal that
// stands in for the host abruptly losing the process (OOM kill, power loss,
// `kill -9`). A fresh instance launched against the exact same configDir and
// socket path must reclaim ownership cleanly:
//   - the stale socket (transport.Unix.Listen's isStaleSocket path) and the
//     flock'd PID file (released by the kernel the instant the old process
//     died) are both reclaimed without any manual cleanup;
//   - the previous Run is still visible with its durable "running" status —
//     reconcileOrphanedReservations deliberately leaves a Run whose run.json
//     already exists untouched (see runs/manager.go's own doc comment: full
//     mid-Step recovery is out of this reconciliation's scope), so it must
//     neither vanish nor be silently fabricated into a false terminal state;
//   - that Run can still be driven to a coherent terminal state through the
//     ordinary cancelRun path, proving the reclaimed instance's index and
//     transport are fully functional, not just "the socket answers pings".
func TestSIGKILLRunnerLevelRecoverySocketAndPIDFile(t *testing.T) {
	skipUnlessUnix(t)
	configDir := t.TempDir()
	socketPath := filepath.Join(configDir, "runner.sock")
	root := seedSigkillWorkspace(t, configDir)
	pidfile := filepath.Join(t.TempDir(), "step.pid")
	env := append(append([]string(nil), os.Environ()...), "TMPDIR="+filepath.Dir(pidfile))

	first := newSigkillDaemon(t, configDir, socketPath, env)
	first.start()
	t.Cleanup(first.stop)

	client := dialSigkillClient(t, socketPath)
	startResponse := client.call("start", "v1/startRun", map[string]any{
		"workspace": root, "capability": "greet", "inputs": map[string]any{"count": 1},
		"approvalMode": "auto", "idempotencyKey": "sigkill-runner-level",
	})
	var started runs.StartResult
	if err := json.Unmarshal(rawResult(t, startResponse), &started); err != nil {
		t.Fatal(err)
	}
	waitForPIDFile(t, pidfile)
	stepPID := readPIDFileInt(t, pidfile)
	if !processAlive(stepPID) {
		t.Fatalf("Step subprocess %d is not running before the kill; test setup is broken", stepPID)
	}

	first.sigkill()

	second := newSigkillDaemon(t, configDir, socketPath, env)
	second.start()
	t.Cleanup(second.stop)
	t.Cleanup(func() {
		if processAlive(stepPID) {
			_ = syscall.Kill(stepPID, syscall.SIGKILL)
		}
	})

	secondClient := dialSigkillClient(t, socketPath)
	getResponse := secondClient.call("get", "v1/getRun", map[string]any{"runId": started.RunID})
	var reloaded runs.GetRunResult
	if err := json.Unmarshal(rawResult(t, getResponse), &reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.Summary.Status != "running" {
		t.Fatalf("reloaded Run status = %q, want running (mid-Step recovery is explicitly out of reconcileOrphanedReservations' scope)", reloaded.Summary.Status)
	}

	cancelResponse := secondClient.call("cancel", "v1/cancelRun", map[string]any{"runId": started.RunID, "reason": "recovered after SIGKILL"})
	if cancelResponse.Err != nil {
		t.Fatalf("cancelRun on the reclaimed instance: %+v", cancelResponse.Err)
	}
	var cancelResult struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rawResult(t, cancelResponse), &cancelResult); err != nil {
		t.Fatal(err)
	}
	if cancelResult.Status != "interrupted" {
		t.Fatalf("cancelRun status = %q, want interrupted", cancelResult.Status)
	}
}

// TestSIGKILLMidStepLeavesOrphanedSubprocessButRunIsStillReconcilable pins
// down this PR's documented policy choice for the mid-Step kill case (the
// desktop-first plan explicitly leaves the exact policy to this PR): killing
// the Runner does not, and today cannot, reach into and terminate a Step's
// own subprocess, because that subprocess runs in its own process group
// (internal/execution/process_unix.go's Setpgid) specifically so the
// *engine's own* cancellation path (context.Cancel -> exec.Cmd.Cancel) can
// kill exactly that Step's tree without touching siblings — a mechanism that
// requires a live engine goroutine to invoke it. A killed Runner has no such
// goroutine, so the Step is simply orphaned (reparented to init) and keeps
// running to completion on its own; Manager.Cancel, called after a restart,
// has no in-memory activeRun for it (m.active is rebuilt empty on every
// startup) and so only ever updates the durable record — it cannot and does
// not attempt to signal a process it no longer has a handle on. This test
// asserts exactly that: the orphaned subprocess survives the kill and the
// restart, while the durable Run record can still be reconciled to a
// terminal state through cancelRun, giving operators a consistent
// bookkeeping view even though the orphaned OS process itself is left for
// the OS (or its own sleep timeout) to eventually reap.
func TestSIGKILLMidStepLeavesOrphanedSubprocessButRunIsStillReconcilable(t *testing.T) {
	skipUnlessUnix(t)
	configDir := t.TempDir()
	socketPath := filepath.Join(configDir, "runner.sock")
	root := seedSigkillWorkspace(t, configDir)
	pidfile := filepath.Join(t.TempDir(), "step.pid")
	env := append(append([]string(nil), os.Environ()...), "TMPDIR="+filepath.Dir(pidfile))

	daemon := newSigkillDaemon(t, configDir, socketPath, env)
	daemon.start()
	t.Cleanup(daemon.stop)

	client := dialSigkillClient(t, socketPath)
	startResponse := client.call("start", "v1/startRun", map[string]any{
		"workspace": root, "capability": "greet", "inputs": map[string]any{"count": 1},
		"approvalMode": "auto", "idempotencyKey": "sigkill-mid-step",
	})
	var started runs.StartResult
	if err := json.Unmarshal(rawResult(t, startResponse), &started); err != nil {
		t.Fatal(err)
	}
	waitForPIDFile(t, pidfile)
	stepPID := readPIDFileInt(t, pidfile)
	t.Cleanup(func() {
		if processAlive(stepPID) {
			_ = syscall.Kill(stepPID, syscall.SIGKILL)
		}
	})

	daemon.sigkill()

	// The documented policy: an orphaned Step subprocess is not reaped by
	// anything in this codebase today. It must still be alive well after its
	// parent Runner died.
	if !processAlive(stepPID) {
		t.Fatalf("Step subprocess %d did not survive the Runner's SIGKILL; if this now fails, PR10's policy comment above needs updating to match the new (improved) behavior", stepPID)
	}

	restarted := newSigkillDaemon(t, configDir, socketPath, env)
	restarted.start()
	t.Cleanup(restarted.stop)

	restartedClient := dialSigkillClient(t, socketPath)
	cancelResponse := restartedClient.call("cancel", "v1/cancelRun", map[string]any{"runId": started.RunID, "reason": "mid-step recovery"})
	if cancelResponse.Err != nil {
		t.Fatalf("cancelRun after restart: %+v", cancelResponse.Err)
	}

	getResponse := restartedClient.call("get", "v1/getRun", map[string]any{"runId": started.RunID})
	var reloaded runs.GetRunResult
	if err := json.Unmarshal(rawResult(t, getResponse), &reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.Summary.Status != "interrupted" {
		t.Fatalf("reloaded Run status after cancelRun = %q, want interrupted", reloaded.Summary.Status)
	}

	// The orphan is still running: the durable record moving to a terminal
	// state is bookkeeping, not proof of the OS process actually stopping.
	if !processAlive(stepPID) {
		t.Fatal("orphaned Step subprocess unexpectedly exited on its own before the assertion ran; sleep duration may be too short for this environment")
	}
}

func waitForPIDFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PID file %s was never written", path)
}

func readPIDFileInt(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse PID file %s: %v", path, err)
	}
	return pid
}
