package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
	"doppels.so/cli/internal/runner/transport"
)

const integrationToken = "runs-rpc-integration-token"

type integrationClient struct {
	conn    net.Conn
	decoder *proto.Decoder
	encoder *proto.Encoder
}

func dialIntegrationClient(t *testing.T, path string) *integrationClient {
	t.Helper()
	conn, err := transport.Unix{}.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	client := &integrationClient{conn: conn, decoder: proto.NewDecoder(conn), encoder: proto.NewEncoder(conn)}
	response := client.call("init", "v1/initialize", map[string]any{
		"token":  integrationToken,
		"client": map[string]any{"name": "cli", "version": "0.0.1"},
	})
	if response.Err != nil {
		t.Fatalf("initialize: %+v", response.Err)
	}
	return client
}

func (c *integrationClient) call(id, method string, params any) *proto.Response {
	if err := c.encoder.WriteFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		panic(err)
	}
	frame, err := c.decoder.ReadFrame()
	if err != nil {
		panic(fmt.Sprintf("read response: %v", err))
	}
	var response proto.Response
	if err := json.Unmarshal(frame, &response); err != nil {
		panic(fmt.Sprintf("decode %s: %v", frame, err))
	}
	return &response
}

func rawResult(t *testing.T, response *proto.Response) json.RawMessage {
	t.Helper()
	raw, ok := response.Result.(json.RawMessage)
	if !ok {
		t.Fatalf("response.Result = %#v, want json.RawMessage", response.Result)
	}
	return raw
}

func (c *integrationClient) readNotification(t *testing.T) (string, json.RawMessage) {
	t.Helper()
	for {
		frame, err := c.decoder.ReadFrame()
		if err != nil {
			t.Fatalf("read notification: %v", err)
		}
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Method == "" {
			continue
		}
		return envelope.Method, envelope.Params
	}
}

func TestSubscribeRunOverRealSocketReplaysThenStreamsLive(t *testing.T) {
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
    run: {shell: sh, script: "touch started; sleep 30; export VALUE=ok"}
    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

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
	serveCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(serveCtx, listener)
	t.Cleanup(srv.Close)

	client := dialIntegrationClient(t, socketPath)
	startResponse := client.call("start", "v1/startRun", map[string]any{
		"workspace": root, "capability": "greet", "inputs": map[string]any{"count": 1},
		"approvalMode": "auto", "idempotencyKey": "integration-1",
	})
	if startResponse.Err != nil {
		t.Fatalf("startRun: %+v", startResponse.Err)
	}
	var started StartResult
	if err := json.Unmarshal(rawResult(t, startResponse), &started); err != nil {
		t.Fatal(err)
	}

	// startRun is deliberately asynchronous. Wait until the fixture Step has
	// started so this test exercises its intended mid-flight subscription,
	// with an initialized on-disk history available for replay.
	waitForFile(t, filepath.Join(root, "started"))
	subResponse := client.call("sub", "v1/subscribeRun", map[string]any{"runId": started.RunID})
	if subResponse.Err != nil {
		t.Fatalf("subscribeRun: %+v", subResponse.Err)
	}
	var snapshot SubscribeResult
	if err := json.Unmarshal(rawResult(t, subResponse), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Events) == 0 || snapshot.Status != "running" {
		t.Fatalf("subscribeRun snapshot = %#v", snapshot)
	}

	cancelResponse := client.call("cancel", "v1/cancelRun", map[string]any{"runId": started.RunID})
	if cancelResponse.Err != nil {
		t.Fatalf("cancelRun: %+v", cancelResponse.Err)
	}

	seenTerminal := false
	deadline := time.Now().Add(5 * time.Second)
	for !seenTerminal && time.Now().Before(deadline) {
		method, params := client.readNotification(t)
		if method != "v1/runEvent" {
			continue
		}
		var event proto.RunEventPayload
		if err := json.Unmarshal(params, &event); err != nil {
			t.Fatal(err)
		}
		if event.Sequence <= snapshot.Events[len(snapshot.Events)-1].Sequence {
			t.Fatalf("live event duplicated replay: %#v (last replay = %#v)", event, snapshot.Events[len(snapshot.Events)-1])
		}
		if event.Type == "run_interrupted" {
			seenTerminal = true
		}
	}
	if !seenTerminal {
		t.Fatal("never observed run_interrupted over the live subscription")
	}
}
