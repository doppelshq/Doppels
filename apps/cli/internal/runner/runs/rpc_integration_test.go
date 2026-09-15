package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
	"doppels.so/cli/internal/runner/transport"
)

const integrationToken = "runs-rpc-integration-token"

type integrationClient struct {
	conn    net.Conn
	decoder *proto.Decoder
	encoder *proto.Encoder

	// queuedNotifications buffers notification frames encountered by call()
	// while it was looking for an RPC response: notifications and responses
	// share one connection, and the server's async forwarder goroutine can
	// enqueue a live v1/runEvent ahead of the RPC response whose handler
	// triggered it (e.g. cancelRun's own broadcast). Without buffering,
	// call() would silently swallow that notification instead of returning
	// it to readNotification, and a test waiting for it would hang forever.
	queuedNotifications []queuedNotification
}

type queuedNotification struct {
	method string
	params json.RawMessage
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
	for {
		frame, err := c.decoder.ReadFrame()
		if err != nil {
			panic(fmt.Sprintf("read response: %v", err))
		}
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			panic(fmt.Sprintf("decode %s: %v", frame, err))
		}
		if envelope.Method != "" {
			c.queuedNotifications = append(c.queuedNotifications, queuedNotification{method: envelope.Method, params: envelope.Params})
			continue
		}
		var response proto.Response
		if err := json.Unmarshal(frame, &response); err != nil {
			panic(fmt.Sprintf("decode %s: %v", frame, err))
		}
		return &response
	}
}

func rawResult(t *testing.T, response *proto.Response) json.RawMessage {
	t.Helper()
	raw, ok := response.Result.(json.RawMessage)
	if !ok {
		t.Fatalf("response.Result = %#v, want json.RawMessage", response.Result)
	}
	return raw
}

func assertSocketPing(t *testing.T, client *integrationClient, id string) {
	t.Helper()
	if response := client.call(id, "v1/ping", map[string]any{}); response.Err != nil {
		t.Fatalf("follow-up ping: %+v", response.Err)
	}
}

func (c *integrationClient) readNotification(t *testing.T) (string, json.RawMessage) {
	t.Helper()
	if len(c.queuedNotifications) > 0 {
		next := c.queuedNotifications[0]
		c.queuedNotifications = c.queuedNotifications[1:]
		return next.method, next.params
	}
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

// TestSubscribeManyTerminalRunsOnOneConnectionDoesNotLeakSubscriptions
// reproduces review finding 5: a long-lived connection that subscribes to
// many Runs, each reaching a terminal state well before the connection
// itself ever closes, must not accumulate unbounded server-side state per
// past subscription. Each iteration reuses the single client connection
// end-to-end (startRun, subscribeRun, cancelRun, drain the terminal
// notification) exactly like a real client would, with no sleeps: every
// step is gated on an explicit signal (status polling already used
// elsewhere in this package, or a notification read from the socket).
func TestSubscribeManyTerminalRunsOnOneConnectionDoesNotLeakSubscriptions(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
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

	const subscriptions = 50
	for i := 0; i < subscriptions; i++ {
		key := fmt.Sprintf("many-terminal-%d", i)
		startResponse := client.call(key, "v1/startRun", map[string]any{
			"workspace": root, "capability": "greet", "inputs": map[string]any{"count": 1},
			"approvalMode": "interactive", "idempotencyKey": key,
		})
		if startResponse.Err != nil {
			t.Fatalf("startRun[%d]: %+v", i, startResponse.Err)
		}
		var started StartResult
		if err := json.Unmarshal(rawResult(t, startResponse), &started); err != nil {
			t.Fatal(err)
		}
		waitForStatus(t, root, started.RunID, "pending_manual")

		subResponse := client.call(key+"-sub", "v1/subscribeRun", map[string]any{"runId": started.RunID})
		if subResponse.Err != nil {
			t.Fatalf("subscribeRun[%d]: %+v", i, subResponse.Err)
		}

		cancelResponse := client.call(key+"-cancel", "v1/cancelRun", map[string]any{"runId": started.RunID})
		if cancelResponse.Err != nil {
			t.Fatalf("cancelRun[%d]: %+v", i, cancelResponse.Err)
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
			if event.RunID == started.RunID && event.Type == "run_cancelled" {
				seenTerminal = true
			}
		}
		if !seenTerminal {
			t.Fatalf("never observed run_cancelled over the live subscription for run %d", i)
		}

		// The subscription for this already-terminal Run must be gone from
		// the domain's own bookkeeping before the next iteration starts —
		// this is the bound that matters: it must not grow with i.
		manager.subsMu.Lock()
		leaked := len(manager.subs[started.RunID])
		manager.subsMu.Unlock()
		if leaked != 0 {
			t.Fatalf("subscriber list for terminated run %d still has %d entries, want 0", i, leaked)
		}
	}

	manager.subsMu.Lock()
	totalTracked := len(manager.subs)
	manager.subsMu.Unlock()
	if totalTracked != 0 {
		t.Fatalf("manager.subs still tracks %d runs after every subscription reached a terminal state, want 0", totalTracked)
	}
}

// TestSubscribeOversizedNonTerminalReplayDoesNotLeakSubscription reproduces
// review finding 7: when subscribeRun's own synchronous replay response
// (not a terminal Run — the isTerminal cleanup in Subscribe never runs)
// turns out to exceed MaxFrameBytes once encoded, dispatch discovers this
// only after Subscribe already returned successfully, having already
// registered the subscriber in Manager.subs and installed its onClose
// callback on the connection. Before Defer grew an onAbort side (review
// finding 7's fix), consumePendingActivation(false) silently dropped that
// activation with no cleanup — a leak identical in shape to review finding
// 5's, just triggered by an oversized reply instead of an unbounded
// connection lifetime. Repeats the scenario several times over one
// held-open real connection and asserts nothing accumulates.
func TestSubscribeOversizedNonTerminalReplayDoesNotLeakSubscription(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
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
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	go srv.Serve(serveCtx, listener)
	t.Cleanup(srv.Close)
	client := dialIntegrationClient(t, socketPath)

	const attempts = 3
	for attempt := 0; attempt < attempts; attempt++ {
		key := fmt.Sprintf("oversized-replay-%d", attempt)
		startResponse := client.call(key, "v1/startRun", map[string]any{
			"workspace": root, "capability": "greet", "inputs": map[string]any{"count": 1},
			"approvalMode": "interactive", "idempotencyKey": key,
		})
		if startResponse.Err != nil {
			t.Fatalf("startRun[%d]: %+v", attempt, startResponse.Err)
		}
		var started StartResult
		if err := json.Unmarshal(rawResult(t, startResponse), &started); err != nil {
			t.Fatal(err)
		}
		waitForStatus(t, root, started.RunID, "pending_manual")
		growEventsPastFrameLimit(t, root, started.RunID)

		subResponse := client.call(key+"-sub", "v1/subscribeRun", map[string]any{"runId": started.RunID})
		if subResponse.Err == nil {
			t.Fatalf("subscribeRun[%d]: oversized non-terminal replay unexpectedly succeeded", attempt)
		}

		manager.subsMu.Lock()
		leaked := len(manager.subs[started.RunID])
		manager.subsMu.Unlock()
		if leaked != 0 {
			t.Fatalf("attempt %d: subscriber list for %s has %d entries after an aborted (oversized) activation, want 0", attempt, started.RunID, leaked)
		}

		// Cancel this Run so the connection has no lingering non-terminal
		// state before the next attempt's startRun.
		if cancelResponse := client.call(key+"-cancel", "v1/cancelRun", map[string]any{"runId": started.RunID}); cancelResponse.Err != nil {
			t.Fatalf("cancelRun[%d]: %+v", attempt, cancelResponse.Err)
		}
	}

	manager.subsMu.Lock()
	totalTracked := len(manager.subs)
	manager.subsMu.Unlock()
	if totalTracked != 0 {
		t.Fatalf("manager.subs still tracks %d runs after every aborted activation, want 0", totalTracked)
	}

	ping := client.call("oversized-replay-ping", "v1/ping", map[string]any{})
	if ping.Err != nil {
		t.Fatalf("connection unusable after an aborted (oversized) subscribeRun activation: %+v", ping.Err)
	}
}

// growEventsPastFrameLimit appends enough well-formed, non-terminal
// RunEvents to runID's durable events.jsonl that its full replay (from
// sequence 0) exceeds proto.MaxFrameBytes once JSON-encoded and wrapped in
// the subscribeRun response envelope. Each appended line stays far under
// runstate's own 1 MiB per-line scan buffer; the total across many lines is
// what crosses the frame limit.
func growEventsPastFrameLimit(t *testing.T, root, runID string) {
	t.Helper()
	path := filepath.Join(root, ".doppels", "runs", runID, "events.jsonl")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sequence := 0
	for _, line := range bytes.Split(bytes.TrimRight(existing, "\n"), []byte("\n")) {
		if len(line) > 0 {
			sequence++
		}
	}
	blob := strings.Repeat("d", 64<<10) // 64 KiB, comfortably under the 1 MiB line cap
	var buf bytes.Buffer
	buf.Write(existing)
	const lines = 80 // 80 * 64 KiB ~= 5 MiB, safely past the 4 MiB frame limit
	for i := 0; i < lines; i++ {
		event := execution.RunEvent{
			APIVersion: execution.APIVersion, Kind: "RunEvent", RunID: runID,
			Sequence: sequence, OccurredAt: time.Now().UTC(), Type: "step_started",
			StepID: "bulk", Data: map[string]any{"blob": blob},
		}
		sequence++
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(encoded)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestListRunsHugeWorkspaceStillYieldsBoundedError reproduces a review
// finding: enqueueError's earlier fix only handled an oversized err.Data,
// assuming err.Message was always one of this codebase's short fixed
// strings. resolveRoot's "workspace not found: " + workspace error breaks
// that assumption — workspace is raw, unbounded request input, so its error
// echoes it straight back into Message with no Data at all. A request whose
// workspace param is large enough that the request itself only just fits
// produces an error response (workspace re-embedded, id, plus the "workspace
// not found: " prefix) larger than the request that produced it, and the
// enqueueError fallback that only strips Data left it unchanged — WriteFrame
// then refused to write it, closing the connection instead of answering.
func TestListRunsHugeWorkspaceStillYieldsBoundedError(t *testing.T) {
	service, _ := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
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
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	go srv.Serve(serveCtx, listener)
	t.Cleanup(srv.Close)
	client := dialIntegrationClient(t, socketPath)

	// Measure this connection's own exact framing overhead (workspace="")
	// for both the request about to be sent and the error resolveRoot would
	// return, then pick the largest workspace for which the request still
	// fits.
	zeroWorkspaceRequest, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "list-1", "method": "v1/listRuns",
		"params": map[string]any{"workspace": ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	zeroWorkspaceError, err := json.Marshal(proto.NewErrorResponse("list-1", &proto.Error{
		Code: proto.CodeWorkspaceNotFound, Message: "workspace not found: ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(zeroWorkspaceError) <= len(zeroWorkspaceRequest) {
		t.Fatalf("test setup invalid: error overhead (%d) must exceed request overhead (%d) to reproduce the bug", len(zeroWorkspaceError), len(zeroWorkspaceRequest))
	}
	workspaceLen := proto.MaxFrameBytes - len(zeroWorkspaceRequest)
	hugeWorkspace := strings.Repeat("w", workspaceLen)

	if err := client.encoder.WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": "list-1", "method": "v1/listRuns",
		"params": map[string]any{"workspace": hugeWorkspace},
	}); err != nil {
		t.Fatalf("request with workspace length %d could not even be sent: %v", workspaceLen, err)
	}
	frame, err := client.decoder.ReadFrame()
	if err != nil {
		t.Fatalf("a huge-workspace request closed the connection instead of returning an in-frame response: %v", err)
	}
	if len(frame) > proto.MaxFrameBytes {
		t.Fatalf("response frame = %d bytes, max %d", len(frame), proto.MaxFrameBytes)
	}
	var response proto.Response
	if err := json.Unmarshal(frame, &response); err != nil {
		t.Fatal(err)
	}
	if response.Err == nil {
		t.Fatal("huge-workspace request unexpectedly returned an unbounded success response")
	}
	idJSON, err := json.Marshal(response.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(idJSON) != `"list-1"` {
		t.Fatalf("response id = %s, want preserved %q", idJSON, "list-1")
	}

	ping := client.call("huge-workspace-ping", "v1/ping", map[string]any{})
	if ping.Err != nil {
		t.Fatalf("connection unusable after huge-workspace response: %+v", ping.Err)
	}
}
