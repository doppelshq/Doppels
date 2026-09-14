package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/runner/proto"
)

// TestDoppelsRunnerBinaryAcceptsHandshake spawns the real binary, dials its
// socket, and walks through v1/initialize + v1/ping. It is the only end-to-
// end proof that the binary bundles the server stack correctly.
func TestDoppelsRunnerBinaryAcceptsHandshake(t *testing.T) {
	configDir := t.TempDir()
	socketPath := filepath.Join(configDir, "runner.sock")
	token := strings.Repeat("a", 64)
	port := pickFreePort(t)
	binaryPath := filepath.Join(t.TempDir(), "doppels-runner")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build runner binary: %v\n%s", err, output)
	}

	cmd := exec.Command(binaryPath,
		"--socket="+socketPath,
		"--token="+token,
		"--config="+configDir,
	)
	cmd.Env = append(os.Environ(),
		"DOPPELS_RUNNER_CONFIG="+configDir,
		fmt.Sprintf("DOPPELS_RUNNER_PORT=%d", port),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	// Wait for the socket to appear.
	deadline := time.Now().Add(3 * time.Second)
	var conn net.Conn
	var err error
	for time.Now().Before(deadline) {
		conn, err = net.DialTimeout("unix", socketPath, 250*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("dial: %v\nstderr: %s", err, stderr.String())
	}
	defer conn.Close()

	encoder := proto.NewEncoder(conn)
	decoder := proto.NewDecoder(conn)

	if err := encoder.WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": "init", "method": "v1/initialize",
		"params": map[string]any{
			"token":  token,
			"client": map[string]any{"name": "smoke", "version": "0.0.0"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	frame, err := decoder.ReadFrame()
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if frame == nil {
		t.Fatalf("no response; stderr: %s", stderr.String())
	}
	var response proto.Response
	if err := json.Unmarshal(frame, &response); err != nil {
		t.Fatal(err)
	}
	if response.Err != nil {
		t.Fatalf("initialize err: %+v", response.Err)
	}
	var initResult proto.InitializeResult
	if err := unmarshalResult(response.Result, &initResult); err != nil {
		t.Fatal(err)
	}
	if initResult.ProtocolVersion != proto.ProtocolVersion {
		t.Fatalf("protocolVersion = %d", initResult.ProtocolVersion)
	}

	if err := encoder.WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": "ping", "method": "v1/ping", "params": map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	frame, err = decoder.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	response = proto.Response{}
	if err := json.Unmarshal(frame, &response); err != nil {
		t.Fatal(err)
	}
	if response.Err != nil {
		t.Fatalf("ping err: %+v", response.Err)
	}

	workspaceRoot := filepath.Join(configDir, "workspace")
	if _, err := project.Init(workspaceRoot); err != nil {
		t.Fatal(err)
	}
	capabilityPath := filepath.Join(workspaceRoot, ".doppels", "capabilities", "greet.yaml")
	capability := `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: greet, version: 1.0.0}
inputs: {}
outputs: {message: {type: string}}
`
	if err := os.WriteFile(capabilityPath, []byte(capability), 0o600); err != nil {
		t.Fatal(err)
	}

	type nodeNotification struct {
		Method string          `json:"method"`
		Params proto.NodeEvent `json:"params"`
	}
	var events []nodeNotification
	call := func(id, method string, params any) proto.Response {
		t.Helper()
		if err := encoder.WriteFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		for {
			frame, err := decoder.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			var notification nodeNotification
			if err := json.Unmarshal(frame, &notification); err != nil {
				t.Fatal(err)
			}
			if notification.Method != "" {
				events = append(events, notification)
				continue
			}
			var got proto.Response
			if err := json.Unmarshal(frame, &got); err != nil {
				t.Fatal(err)
			}
			return got
		}
	}

	subscribe := call("subscribe", "v1/subscribeNode", map[string]any{})
	if subscribe.Err != nil {
		t.Fatalf("subscribeNode err: %+v", subscribe.Err)
	}
	var initial proto.NodeStatus
	if err := unmarshalResult(subscribe.Result, &initial); err != nil {
		t.Fatal(err)
	}
	if len(initial.Workspaces) != 0 {
		t.Fatalf("initial workspaces = %+v, want empty", initial.Workspaces)
	}

	added := call("add", "v1/addWorkspace", map[string]any{"root": workspaceRoot})
	if added.Err != nil {
		t.Fatalf("addWorkspace err: %+v", added.Err)
	}
	if len(events) != 1 || events[0].Method != "v1/nodeEvent" || events[0].Params.Kind != proto.NodeEventWorkspaceAdded {
		t.Fatalf("add events = %+v", events)
	}
	resubscribed := call("resubscribe", "v1/subscribeNode", map[string]any{})
	var subscribedStatus proto.NodeStatus
	if resubscribed.Err != nil || unmarshalResult(resubscribed.Result, &subscribedStatus) != nil || len(subscribedStatus.Workspaces) != 1 || subscribedStatus.Workspaces[0].Capabilities != 1 {
		t.Fatalf("subscribeNode snapshot = %#v, decoded = %+v", resubscribed, subscribedStatus)
	}

	statusResponse := call("status", "v1/getNodeStatus", map[string]any{})
	var status proto.NodeStatus
	if statusResponse.Err != nil || unmarshalResult(statusResponse.Result, &status) != nil || len(status.Workspaces) != 1 || status.Workspaces[0].Capabilities != 1 {
		t.Fatalf("getNodeStatus = %#v, decoded = %+v", statusResponse, status)
	}

	listed := call("caps", "v1/listCapabilities", map[string]any{})
	var capabilities []proto.CapabilitySummary
	if listed.Err != nil || unmarshalResult(listed.Result, &capabilities) != nil || len(capabilities) != 1 || capabilities[0].Name != "greet" {
		t.Fatalf("listCapabilities = %#v, decoded = %+v", listed, capabilities)
	}

	detail := call("cap", "v1/getCapability", map[string]any{"workspace": workspaceRoot, "name": "greet", "version": "1.0.0"})
	if detail.Err != nil {
		t.Fatalf("getCapability err: %+v", detail.Err)
	}

	removed := call("remove", "v1/removeWorkspace", map[string]any{"root": workspaceRoot})
	if removed.Err != nil {
		t.Fatalf("removeWorkspace err: %+v", removed.Err)
	}
	if len(events) != 2 || events[1].Method != "v1/nodeEvent" || events[1].Params.Kind != proto.NodeEventWorkspaceRemoved {
		t.Fatalf("remove events = %+v", events)
	}
	// Capabilities advertised for future F2 workstreams.
	for _, capability := range initResult.Capabilities {
		if capability == "" {
			t.Fatalf("empty capability advertised")
		}
	}
}

// TestDoppelsRunnerEmitsRunStartedAndRunFinishedNodeEvents reproduces review
// finding 11: cmd/doppels-runner built its runs.Manager without wiring
// Config.OnStarted/OnFinished, so v1/nodeEvent {kind: runStarted|
// runFinished} (RFC §10) was never emitted by the real daemon for genuine
// Runs — only workspace-related node events were reachable. It also proves
// the official handshake client name "doppels-cli" (this codebase's
// established User-Agent convention; the RFC's own handshake example in
// docs/runner-protocol.md §6 uses "doppels-desktop") is recorded as the
// Run's source rather than falling through to "local".
func TestDoppelsRunnerEmitsRunStartedAndRunFinishedNodeEvents(t *testing.T) {
	configDir := t.TempDir()
	socketPath := filepath.Join(configDir, "runner.sock")
	token := strings.Repeat("b", 64)
	binaryPath := filepath.Join(t.TempDir(), "doppels-runner")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build runner binary: %v\n%s", err, output)
	}

	cmd := exec.Command(binaryPath, "--socket="+socketPath, "--token="+token, "--config="+configDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	var conn net.Conn
	var err error
	for time.Now().Before(deadline) {
		conn, err = net.DialTimeout("unix", socketPath, 250*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("dial: %v\nstderr: %s", err, stderr.String())
	}
	defer conn.Close()

	encoder := proto.NewEncoder(conn)
	decoder := proto.NewDecoder(conn)

	if err := encoder.WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": "init", "method": "v1/initialize",
		"params": map[string]any{"token": token, "client": map[string]any{"name": "doppels-cli", "version": "0.0.0"}},
	}); err != nil {
		t.Fatal(err)
	}
	initFrame, err := decoder.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	var initResponse proto.Response
	if err := json.Unmarshal(initFrame, &initResponse); err != nil {
		t.Fatal(err)
	}
	if initResponse.Err != nil {
		t.Fatalf("initialize err: %+v", initResponse.Err)
	}

	workspaceRoot := filepath.Join(configDir, "workspace")
	if _, err := project.Init(workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, ".doppels", "capabilities", "greet.yaml"), []byte(`apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: greet, version: 1.0.0}
inputs: {}
outputs: {value: {type: string}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, ".doppels", "recipes", "greet.yaml"), []byte(`apiVersion: doppels.so/v1alpha1
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
`), 0o600); err != nil {
		t.Fatal(err)
	}

	type nodeNotification struct {
		Method string          `json:"method"`
		Params proto.NodeEvent `json:"params"`
	}
	var kinds []string
	call := func(id, method string, params any) proto.Response {
		t.Helper()
		if err := encoder.WriteFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			frame, err := decoder.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			var notification nodeNotification
			if err := json.Unmarshal(frame, &notification); err != nil {
				t.Fatal(err)
			}
			if notification.Method != "" {
				if notification.Method == "v1/nodeEvent" {
					kinds = append(kinds, notification.Params.Kind)
				}
				continue
			}
			var got proto.Response
			if err := json.Unmarshal(frame, &got); err != nil {
				t.Fatal(err)
			}
			return got
		}
		t.Fatal("timed out waiting for a response")
		return proto.Response{}
	}

	if r := call("subscribe", "v1/subscribeNode", map[string]any{}); r.Err != nil {
		t.Fatalf("subscribeNode: %+v", r.Err)
	}
	if r := call("add", "v1/addWorkspace", map[string]any{"root": workspaceRoot}); r.Err != nil {
		t.Fatalf("addWorkspace: %+v", r.Err)
	}

	started := call("start", "v1/startRun", map[string]any{
		"workspace": workspaceRoot, "capability": "greet", "inputs": map[string]any{},
		"approvalMode": "auto", "idempotencyKey": "e2e-node-events",
	})
	if started.Err != nil {
		t.Fatalf("startRun: %+v", started.Err)
	}
	var startResult struct {
		RunID string `json:"runId"`
	}
	if err := unmarshalResult(started.Result, &startResult); err != nil {
		t.Fatal(err)
	}

	// Wait for run_finished to actually reach the wire: node events are
	// notifications, decoupled from the startRun response.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		hasStarted, hasFinished := false, false
		for _, kind := range kinds {
			if kind == proto.NodeEventRunStarted {
				hasStarted = true
			}
			if kind == proto.NodeEventRunFinished {
				hasFinished = true
			}
		}
		if hasStarted && hasFinished {
			break
		}
		call("poll", "v1/ping", map[string]any{})
	}
	foundStarted, foundFinished := false, false
	startedIndex, finishedIndex := -1, -1
	for i, kind := range kinds {
		if kind == proto.NodeEventRunStarted {
			foundStarted, startedIndex = true, i
		}
		if kind == proto.NodeEventRunFinished {
			foundFinished, finishedIndex = true, i
		}
	}
	if !foundStarted || !foundFinished {
		t.Fatalf("node event kinds = %v, want runStarted and runFinished", kinds)
	}
	if startedIndex > finishedIndex {
		t.Fatalf("runFinished (index %d) arrived before runStarted (index %d)", finishedIndex, startedIndex)
	}

	got := call("get", "v1/getRun", map[string]any{"runId": startResult.RunID})
	if got.Err != nil {
		t.Fatalf("getRun: %+v", got.Err)
	}
	var summary struct {
		Summary struct {
			Source string `json:"source"`
			Status string `json:"status"`
		} `json:"summary"`
	}
	if err := unmarshalResult(got.Result, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Summary.Source != "cli" {
		t.Fatalf("Run source = %q, want %q for handshake client.name %q", summary.Summary.Source, "cli", "doppels-cli")
	}
	if summary.Summary.Status != "succeeded" {
		t.Fatalf("Run status = %q, want succeeded", summary.Summary.Status)
	}
}

func TestResolveTokenRejectsMalformedPersistedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.token")
	if err := os.WriteFile(path, []byte(strings.Repeat("0", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveToken("", path); err == nil {
		t.Fatal("expected malformed token to be rejected")
	}
}

func TestRunRejectsCorruptWorkspaceRegistryBeforeListening(t *testing.T) {
	configDir := t.TempDir()
	socketPath := filepath.Join(configDir, "runner.sock")
	if err := os.WriteFile(filepath.Join(configDir, "workspaces.json"), []byte(`{"version":2,"roots":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(socketPath, strings.Repeat("a", 64), "test", configDir)
	if err == nil {
		t.Fatal("run accepted corrupt workspace registry")
	}
	if _, statErr := os.Lstat(socketPath); !os.IsNotExist(statErr) {
		t.Fatalf("runner listened before validating registry: %v", statErr)
	}
}

func unmarshalResult(value any, out any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func pickFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

var _ = context.Background

// TestExistingRunnerRequiresAuthenticatedHandshake pins what "another runner
// already owns this socket" means: a successful v1/initialize result with our
// token, not merely something that answered. Anything else must let the
// startup fail loudly instead of exiting 0 and leaving the user with no
// runner at all.
func TestExistingRunnerRequiresAuthenticatedHandshake(t *testing.T) {
	token := strings.Repeat("a", 64)
	tests := []struct {
		name  string
		reply string
		want  bool
	}{
		{name: "empty object", reply: `{}`, want: false},
		{name: "auth error", reply: `{"jsonrpc":"2.0","id":"probe","error":{"code":-32001,"message":"unauthorized"}}`, want: false},
		{name: "other id", reply: `{"jsonrpc":"2.0","id":"other","result":{"protocolVersion":1,"runnerVersion":"x"}}`, want: false},
		{name: "wrong jsonrpc", reply: `{"jsonrpc":"1.0","id":"probe","result":{"protocolVersion":1,"runnerVersion":"x"}}`, want: false},
		{name: "not json", reply: `garbage`, want: false},
		{name: "authenticated", reply: fmt.Sprintf(`{"jsonrpc":"2.0","id":"probe","result":{"protocolVersion":%d,"runnerVersion":"0.1.0-dev"}}`, proto.ProtocolVersion), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			socketPath := filepath.Join(t.TempDir(), "runner.sock")
			listener, err := net.Listen("unix", socketPath)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				if _, err := proto.NewDecoder(conn).ReadFrame(); err != nil {
					return
				}
				_, _ = conn.Write([]byte(tt.reply + "\n"))
			}()
			if got := existingRunner(token, socketPath); got != tt.want {
				t.Fatalf("existingRunner = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestExistingRunnerProbeIsBounded pins that a peer which accepts and then
// stays silent cannot hang the runner's startup forever.
func TestExistingRunnerProbeIsBounded(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runner.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()
	done := make(chan bool, 1)
	go func() { done <- existingRunner(strings.Repeat("a", 64), socketPath) }()
	conn := <-accepted
	defer conn.Close()
	select {
	case alive := <-done:
		if alive {
			t.Fatal("a silent peer must not count as a live runner")
		}
	case <-time.After(probeTimeout + 2*time.Second):
		t.Fatal("probe never timed out")
	}
}

// TestResolveTokenRejectsLooseTokenPermissions pins that a token readable by
// other users is not silently trusted: it is the whole authority to execute
// Steps on this host.
func TestResolveTokenRejectsLooseTokenPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.token")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveToken("", path); err == nil {
		t.Fatal("world-readable token accepted")
	}
}

// TestResolveTokenIsAtomicUnderConcurrency pins the startup race: several
// runners (or a supervisor restarting one) can call resolveToken at the same
// time on a fresh config dir. Every caller must end up with the same valid
// token; none may observe a half-written file.
func TestResolveTokenIsAtomicUnderConcurrency(t *testing.T) {
	for round := 0; round < 20; round++ {
		path := filepath.Join(t.TempDir(), "runner.token")
		gate := make(chan struct{})
		type outcome struct {
			token string
			err   error
		}
		results := make(chan outcome, 16)
		for worker := 0; worker < 16; worker++ {
			go func() {
				<-gate
				token, err := resolveToken("", path)
				results <- outcome{token: token, err: err}
			}()
		}
		close(gate)
		first := ""
		for worker := 0; worker < 16; worker++ {
			got := <-results
			if got.err != nil {
				t.Fatalf("round %d: %v", round, got.err)
			}
			if first == "" {
				first = got.token
			}
			if got.token != first {
				t.Fatalf("round %d: concurrent callers disagree: %s vs %s", round, first, got.token)
			}
		}
		persisted, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(persisted) != first {
			t.Fatalf("persisted token %q != returned %q", persisted, first)
		}
	}
}
