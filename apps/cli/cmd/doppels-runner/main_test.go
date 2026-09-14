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
	token := "smoke-token-fixed"
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
