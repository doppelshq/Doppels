package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
	"doppels.so/cli/internal/runner/transport"
)

func TestSubscribeRunLogsStreamsOverRealSocket(t *testing.T) {
	manager, root, client := startLiveLogIntegrationServer(t, `
touch live-ready
sleep 0.2
printf 'stdout-one\n'
printf 'stdout-two\n'
printf 'stderr-one\n' >&2
export VALUE=ok
`)
	started := startLiveLogRunOverSocket(t, client, root, "live-socket")
	waitForFile(t, filepath.Join(root, "live-ready"))
	response := client.call("logs-sub", "v1/subscribeRunLogs", map[string]any{"runId": started.RunID, "stepId": "run"})
	if response.Err != nil {
		t.Fatalf("subscribeRunLogs: %+v", response.Err)
	}
	var subscription LiveLogSubscription
	if err := json.Unmarshal(rawResult(t, response), &subscription); err != nil {
		t.Fatal(err)
	}
	if !subscription.Active {
		t.Fatal("subscribeRunLogs returned inactive for a running Step")
	}

	wantStdout := []byte("stdout-one\nstdout-two\n")
	wantStderr := []byte("stderr-one\n")
	var stdout, stderr bytes.Buffer
	deadline := time.Now().Add(5 * time.Second)
	for (stdout.Len() < len(wantStdout) || stderr.Len() < len(wantStderr)) && time.Now().Before(deadline) {
		method, params := client.readNotification(t)
		if method != "v1/runLog" {
			continue
		}
		var chunk proto.RunLogChunk
		if err := json.Unmarshal(params, &chunk); err != nil {
			t.Fatal(err)
		}
		if chunk.RunID != started.RunID || chunk.StepID != "run" {
			t.Fatalf("chunk identity = %#v", chunk)
		}
		switch chunk.Stream {
		case "stdout":
			stdout.WriteString(chunk.Data)
		case "stderr":
			stderr.WriteString(chunk.Data)
		default:
			t.Fatalf("unknown stream %q", chunk.Stream)
		}
	}
	if !bytes.Equal(stdout.Bytes(), wantStdout) || !bytes.Equal(stderr.Bytes(), wantStderr) {
		t.Fatalf("live stdout/stderr = %q / %q", stdout.Bytes(), stderr.Bytes())
	}
	waitForStatus(t, root, started.RunID, "succeeded")
	diskStdout, err := os.ReadFile(filepath.Join(root, ".doppels", "runs", started.RunID, "logs", "run.stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	diskStderr, err := os.ReadFile(filepath.Join(root, ".doppels", "runs", started.RunID, "logs", "run.stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), diskStdout) || !bytes.Equal(stderr.Bytes(), diskStderr) {
		t.Fatalf("live logs differ from disk: stdout %q/%q stderr %q/%q", stdout.Bytes(), diskStdout, stderr.Bytes(), diskStderr)
	}
	_ = manager
}

func TestSubscribeRunLogsDisconnectsCleanly(t *testing.T) {
	manager, root, client := startLiveLogIntegrationServer(t, `
touch live-ready
sleep 30
export VALUE=ok
`)
	started := startLiveLogRunOverSocket(t, client, root, "live-disconnect")
	waitForFile(t, filepath.Join(root, "live-ready"))
	response := client.call("logs-sub", "v1/subscribeRunLogs", map[string]any{"runId": started.RunID})
	if response.Err != nil {
		t.Fatalf("subscribeRunLogs: %+v", response.Err)
	}
	waitForLogSubscriberCount(t, manager, started.RunID, 1)
	if err := client.conn.Close(); err != nil {
		t.Fatal(err)
	}
	waitForLogSubscriberCount(t, manager, started.RunID, 0)
	if _, rpcErr := manager.Cancel(started.RunID, "test complete"); rpcErr != nil {
		t.Fatalf("Cancel: %+v", rpcErr)
	}
}

func startLiveLogIntegrationServer(t *testing.T, script string) (*Manager, string, *integrationClient) {
	t.Helper()
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
    run:
      shell: sh
      script: |
`+indentYAML(script, 8)+`    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)
	manager := NewManager(context.Background(), service, Config{
		NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")},
	})
	srv := server.New(server.Config{
		Token: integrationToken, RunnerVersion: "0.0.1-test",
		NodeStatus: func() proto.NodeStatus { return proto.NodeStatus{} },
		Log:        func(string, ...any) {},
	})
	RegisterRPC(srv, manager)
	socketPath := filepath.Join(t.TempDir(), "runner.sock")
	listener, err := transport.Unix{}.Listen(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(context.Background(), listener)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { manager.Close() })
	return manager, root, dialIntegrationClient(t, socketPath)
}

func startLiveLogRunOverSocket(t *testing.T, client *integrationClient, root, key string) StartResult {
	t.Helper()
	response := client.call("start-"+key, "v1/startRun", map[string]any{
		"workspace": root, "capability": "greet", "inputs": map[string]any{"count": 1},
		"approvalMode": "auto", "idempotencyKey": key,
	})
	if response.Err != nil {
		t.Fatalf("startRun: %+v", response.Err)
	}
	var started StartResult
	if err := json.Unmarshal(rawResult(t, response), &started); err != nil {
		t.Fatal(err)
	}
	return started
}

func indentYAML(value string, spaces int) string {
	prefix := string(bytes.Repeat([]byte(" "), spaces))
	lines := bytes.Split([]byte(value), []byte("\n"))
	var result bytes.Buffer
	for _, line := range lines {
		if len(line) > 0 {
			result.WriteString(prefix)
			result.Write(line)
		}
		result.WriteByte('\n')
	}
	return result.String()
}
