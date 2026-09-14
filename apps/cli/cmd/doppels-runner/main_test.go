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

	cmd := exec.Command("/tmp/doppels-runner",
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
	// Capabilities advertised for future F2 workstreams.
	for _, capability := range initResult.Capabilities {
		if capability == "" {
			t.Fatalf("empty capability advertised")
		}
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
