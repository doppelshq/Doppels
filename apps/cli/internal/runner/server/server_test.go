package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/transport"
)

const testToken = "test-runner-token"

func testConfig() Config {
	return Config{
		Token:            testToken,
		RunnerVersion:    "0.1.0-test",
		HandshakeTimeout: 10 * time.Second,
		NodeStatus: func() proto.NodeStatus {
			return proto.NodeStatus{
				State:           "online",
				RunnerVersion:   "0.1.0-test",
				ProtocolVersion: proto.ProtocolVersion,
				StartedAt:       "2026-09-13T10:00:00Z",
				Workspaces:      []proto.WorkspaceSummary{},
			}
		},
		Log: func(string, ...any) {},
	}
}

type testServer struct {
	*Server
	path string
	done chan error
}

func startServer(t *testing.T, cfg Config) *testServer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.sock")
	listener, err := transport.Unix{}.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	server := New(cfg)
	ts := &testServer{Server: server, path: path, done: make(chan error, 1)}
	go func() {
		ts.done <- server.Serve(context.Background(), listener)
	}()
	t.Cleanup(func() {
		server.Close()
	})
	return ts
}

type testClient struct {
	conn    net.Conn
	decoder *proto.Decoder
	encoder *proto.Encoder
}

func dialClient(t *testing.T, ts *testServer) *testClient {
	t.Helper()
	conn, err := transport.Unix{}.Dial(ts.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &testClient{conn: conn, decoder: proto.NewDecoder(conn), encoder: proto.NewEncoder(conn)}
}

func (c *testClient) call(id any, method string, params any) *proto.Response {
	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		request["params"] = params
	}
	if err := c.encoder.WriteFrame(request); err != nil {
		panic(err)
	}
	return c.readResponse()
}

func (c *testClient) readResponse() *proto.Response {
	frame, err := c.decoder.ReadFrame()
	if err != nil {
		panic(fmt.Sprintf("read response: %v", err))
	}
	var response proto.Response
	if err := json.Unmarshal(frame, &response); err != nil {
		panic(fmt.Sprintf("decode response %s: %v", frame, err))
	}
	return &response
}

func (c *testClient) initialize() *proto.Response {
	return c.call("init-1", "v1/initialize", map[string]any{
		"token":  testToken,
		"client": map[string]any{"name": "doppels-test", "version": "0.0.1"},
	})
}

func (c *testClient) readNotification(t *testing.T) (method string, params json.RawMessage) {
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
			t.Fatalf("decode frame %s: %v", frame, err)
		}
		if envelope.Method == "" {
			continue // response to an unrelated id
		}
		return envelope.Method, envelope.Params
	}
}

func TestServerRejectsMethodsBeforeInitialize(t *testing.T) {
	ts := startServer(t, testConfig())
	client := dialClient(t, ts)

	response := client.call(1, "v1/ping", map[string]any{})
	if response.Err == nil || response.Err.Code != proto.CodeNotInitialized {
		t.Fatalf("err = %+v, want -32000", response.Err)
	}
}

func TestServerNewRejectsEmptyToken(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty Token")
		}
	}()
	_ = New(Config{NodeStatus: func() proto.NodeStatus { return proto.NodeStatus{} }})
}

func TestServerInitializeRejectsEmptyClientToken(t *testing.T) {
	ts := startServer(t, testConfig())
	client := dialClient(t, ts)

	response := client.call("bad", "v1/initialize", map[string]any{
		"token":  "",
		"client": map[string]any{"name": "x", "version": "0"},
	})
	if response.Err == nil || response.Err.Code != proto.CodeAuthFailed {
		t.Fatalf("err = %+v, want -32001 (empty token rejected)", response.Err)
	}
}

func TestServerInitializeAuthenticatesToken(t *testing.T) {
	ts := startServer(t, testConfig())
	client := dialClient(t, ts)

	bad := client.call("bad", "v1/initialize", map[string]any{
		"token":  "wrong-token",
		"client": map[string]any{"name": "x", "version": "0"},
	})
	if bad.Err == nil || bad.Err.Code != proto.CodeAuthFailed {
		t.Fatalf("err = %+v, want -32001", bad.Err)
	}

	good := client.initialize()
	if good.Err != nil {
		t.Fatalf("initialize failed: %+v", good.Err)
	}
	var result proto.InitializeResult
	decodeResult(t, good, &result)
	if result.ProtocolVersion != proto.ProtocolVersion {
		t.Fatalf("protocolVersion = %d", result.ProtocolVersion)
	}
	if result.RunnerVersion != "0.1.0-test" {
		t.Fatalf("runnerVersion = %s", result.RunnerVersion)
	}
	if result.NodeStatus.State != "online" {
		t.Fatalf("nodeStatus.state = %s", result.NodeStatus.State)
	}
}

func TestServerInitializeRejectsUnsupportedClientProtocol(t *testing.T) {
	ts := startServer(t, testConfig())
	client := dialClient(t, ts)

	response := client.call("vm", "v1/initialize", map[string]any{
		"token":           testToken,
		"protocolVersion": 99,
		"client":          map[string]any{"name": "x", "version": "0"},
	})
	if response.Err == nil || response.Err.Code != proto.CodeVersionMismatch {
		t.Fatalf("err = %+v, want -32002", response.Err)
	}
	var data struct {
		Expected  int `json:"expected"`
		Supported int `json:"supported"`
	}
	raw, _ := json.Marshal(response.Err.Data)
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data.Expected != 99 || data.Supported != proto.ProtocolVersion {
		t.Fatalf("versionMismatch data = %+v", data)
	}
}

func TestServerPingAfterHandshake(t *testing.T) {
	ts := startServer(t, testConfig())
	client := dialClient(t, ts)
	if response := client.initialize(); response.Err != nil {
		t.Fatalf("initialize: %+v", response.Err)
	}

	response := client.call("p1", "v1/ping", map[string]any{})
	if response.Err != nil {
		t.Fatalf("ping: %+v", response.Err)
	}
	var result struct {
		Pong string `json:"pong"`
	}
	decodeResult(t, response, &result)
	if result.Pong == "" {
		t.Fatalf("pong = %q", result.Pong)
	}
}

func TestServerUnknownMethod(t *testing.T) {
	ts := startServer(t, testConfig())
	client := dialClient(t, ts)
	if response := client.initialize(); response.Err != nil {
		t.Fatalf("initialize: %+v", response.Err)
	}

	response := client.call("u1", "v1/nope", map[string]any{})
	if response.Err == nil || response.Err.Code != proto.CodeMethodNotFound {
		t.Fatalf("err = %+v, want -32601", response.Err)
	}
}

func TestServerGetNodeStatus(t *testing.T) {
	ts := startServer(t, testConfig())
	client := dialClient(t, ts)
	if response := client.initialize(); response.Err != nil {
		t.Fatalf("initialize: %+v", response.Err)
	}

	response := client.call("s1", "v1/getNodeStatus", map[string]any{})
	if response.Err != nil {
		t.Fatalf("getNodeStatus: %+v", response.Err)
	}
	var status proto.NodeStatus
	decodeResult(t, response, &status)
	if status.State != "online" || status.RunnerVersion != "0.1.0-test" {
		t.Fatalf("status = %+v", status)
	}
}

func TestServerSubscribeNodeSnapshotThenEvents(t *testing.T) {
	ts := startServer(t, testConfig())
	client := dialClient(t, ts)
	if response := client.initialize(); response.Err != nil {
		t.Fatalf("initialize: %+v", response.Err)
	}

	response := client.call("n1", "v1/subscribeNode", map[string]any{})
	if response.Err != nil {
		t.Fatalf("subscribeNode: %+v", response.Err)
	}
	var snapshot proto.NodeStatus
	decodeResult(t, response, &snapshot)
	if snapshot.ProtocolVersion != proto.ProtocolVersion {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	ts.EmitNodeEvent(proto.NodeEvent{Kind: proto.NodeEventRunStarted, Payload: map[string]any{"runId": "r-1"}})

	method, params := client.readNotification(t)
	if method != "v1/nodeEvent" {
		t.Fatalf("method = %s", method)
	}
	var event proto.NodeEvent
	if err := json.Unmarshal(params, &event); err != nil {
		t.Fatal(err)
	}
	if event.Kind != proto.NodeEventRunStarted {
		t.Fatalf("kind = %s", event.Kind)
	}
}

func TestSubscribeNodeIsNotActiveBeforeSnapshotDispatch(t *testing.T) {

	s := New(testConfig())
	conn := newConnection(s, nil)

	if _, protoErr := s.handleSubscribeNode(conn, nil); protoErr != nil {
		t.Fatalf("subscribeNode: %+v", protoErr)
	}
	conn.mu.Lock()
	subscribed := conn.nodeSubscribed
	conn.mu.Unlock()
	if subscribed {
		t.Fatal("subscription became active before its snapshot was dispatched")
	}
}

func TestServerUnsubscribedClientReceivesNoNodeEvents(t *testing.T) {
	ts := startServer(t, testConfig())
	client := dialClient(t, ts)
	if response := client.initialize(); response.Err != nil {
		t.Fatalf("initialize: %+v", response.Err)
	}
	// No subscribeNode call.
	ts.EmitNodeEvent(proto.NodeEvent{Kind: proto.NodeEventRunFinished, Payload: map[string]any{}})

	// The only way to observe silence safely: send a ping and expect the
	// next frame to be its response, not a notification.
	response := client.call("quiet", "v1/ping", map[string]any{})
	if response.Err != nil {
		t.Fatalf("ping: %+v", response.Err)
	}
}

// TestDeliverRunEventReportsDropAndDeliverRunGapClosesOnSaturation
// reproduces review finding 3: a saturated outbound queue silently dropped
// both v1/runEvent and its v1/nodeEvent runEventGap escape hatch, so a
// stuck client could miss events with no signal at all — not even the
// "unmistakable resync" of a closed connection. DeliverRunEvent must report
// whether the frame was actually queued so the caller (runs.Manager) can
// react, and DeliverRunGap must be delivered or force a close: it is the
// client's only signal to resynchronize, so it may never itself be a
// silent, best-effort drop. A saturated *real* connection object is used
// (no fake subscriber) since the bug is specifically in the outbound queue.
func TestDeliverRunEventReportsDropAndDeliverRunGapClosesOnSaturation(t *testing.T) {
	server := New(testConfig())
	clientEnd, _ := net.Pipe()
	defer clientEnd.Close()
	conn := newConnection(server, clientEnd)

	for i := 0; i < outboundBufferSize; i++ {
		if !conn.enqueue(map[string]any{"filler": i}, false) {
			t.Fatalf("failed to fill outbound queue at slot %d", i)
		}
	}

	if conn.DeliverRunEvent(proto.RunEventPayload{RunID: "r-1", Sequence: 0, Type: "run_created"}) {
		t.Fatal("DeliverRunEvent reported success while the outbound queue was saturated")
	}
	if conn.isClosed() {
		t.Fatal("a dropped best-effort runEvent must not by itself close the connection")
	}

	conn.DeliverRunGap("r-1", 0)
	if !conn.isClosed() {
		t.Fatal("DeliverRunGap on a saturated queue must guarantee delivery or close the connection — it did neither")
	}
}

// TestSubscribeResponseIsQueuedBeforeAnyDeferredLiveEvent reproduces review
// finding 4's first half: a domain handler that starts forwarding live
// events synchronously (inside the handler, before its RPC response is
// enqueued) can lose the ordering guarantee "replay/snapshot first, live
// events after" — a live notification can win the race for the connection's
// outbound queue and reach the wire before the RPC response that is
// supposed to precede it. RunEventSubscriber.Defer lets the handler hand off
// activation of live delivery to run only once dispatch has confirmed the
// response was queued (mirroring the existing v1/subscribeNode pattern).
func TestSubscribeResponseIsQueuedBeforeAnyDeferredLiveEvent(t *testing.T) {
	ts := startServer(t, testConfig())
	ts.HandleSubscribe("v1/subscribeRun", func(sub RunEventSubscriber, params []byte) (any, *proto.Error) {
		// A handler using Defer correctly: activation of live delivery must
		// not race the RPC response onto the wire.
		sub.Defer(func() {
			sub.DeliverRunEvent(proto.RunEventPayload{RunID: "r-1", Sequence: 5, Type: "step_started"})
		})
		return map[string]any{"status": "running"}, nil
	})

	client := dialClient(t, ts)
	if response := client.initialize(); response.Err != nil {
		t.Fatalf("initialize: %+v", response.Err)
	}
	if err := client.encoder.WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": "sub-1", "method": "v1/subscribeRun",
		"params": map[string]any{"runId": "r-1"},
	}); err != nil {
		t.Fatal(err)
	}

	first, err := client.decoder.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(first, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Method != "" {
		t.Fatalf("first frame after subscribeRun was a %q notification, want the RPC response first", envelope.Method)
	}

	method, _ := client.readNotification(t)
	if method != "v1/runEvent" {
		t.Fatalf("second frame method = %s, want the deferred v1/runEvent", method)
	}
}

// TestConnectionCloseUnsubscribesRunEventSubscriptions reproduces review
// finding 4's second half: NotifyClosed must let a domain subscriber (e.g.
// runs.Manager) learn a connection died so it can unsubscribe, instead of
// leaking a subscriber-list entry (and its forwarder goroutine) forever.
func TestConnectionCloseUnsubscribesRunEventSubscriptions(t *testing.T) {
	server := New(testConfig())
	clientEnd, _ := net.Pipe()
	conn := newConnection(server, clientEnd)

	notified := make(chan struct{}, 1)
	conn.NotifyClosed(func() { notified <- struct{}{} })

	conn.close()

	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("NotifyClosed callback never ran after the connection closed")
	}
}

// TestNotifyClosedRunsImmediatelyOnAnAlreadyClosedConnection guards the
// registration-after-close race: a subscriber that registers cleanup after
// the connection already died must still get its callback, or it leaks
// forever waiting for a close that already happened.
func TestNotifyClosedRunsImmediatelyOnAnAlreadyClosedConnection(t *testing.T) {
	server := New(testConfig())
	clientEnd, _ := net.Pipe()
	conn := newConnection(server, clientEnd)
	conn.close()

	notified := make(chan struct{}, 1)
	conn.NotifyClosed(func() { notified <- struct{}{} })

	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("NotifyClosed registered on an already-closed connection was never invoked")
	}
}

// TestNotifyClosedUnregisterBoundsCallbacksOnALongLivedConnection reproduces
// review finding 5: a connection that outlives many Run subscriptions (each
// terminating on its own, long before the connection itself closes) must not
// accumulate one stale onClose callback per past subscription forever. The
// returned unregister func is how a domain subscriber (runs.Manager) releases
// its callback as soon as its own subscription ends; unregistering must
// actually shrink the connection's bookkeeping, not just prevent double
// delivery.
func TestNotifyClosedUnregisterBoundsCallbacksOnALongLivedConnection(t *testing.T) {
	server := New(testConfig())
	clientEnd, _ := net.Pipe()
	conn := newConnection(server, clientEnd)

	const subscriptions = 500
	var fired atomic.Int32
	var unregisters []func()
	for i := 0; i < subscriptions; i++ {
		unregister := conn.NotifyClosed(func() { fired.Add(1) })
		unregisters = append(unregisters, unregister)
	}
	conn.mu.Lock()
	registered := len(conn.onClose)
	conn.mu.Unlock()
	if registered != subscriptions {
		t.Fatalf("registered onClose callbacks = %d, want %d", registered, subscriptions)
	}

	// Every subscription "finishes" on its own (Run reaches a terminal
	// state) long before the connection closes — exactly the long-lived
	// connection scenario from the finding.
	for _, unregister := range unregisters {
		unregister()
	}
	conn.mu.Lock()
	remaining := len(conn.onClose)
	conn.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("onClose callbacks after unregistering all = %d, want 0 (leak: bookkeeping grows without bound on a long-lived connection)", remaining)
	}

	// A late registration after everything else unregistered must still work
	// normally, and closing the connection must not resurrect any
	// unregistered callback.
	lateNotified := make(chan struct{}, 1)
	conn.NotifyClosed(func() { lateNotified <- struct{}{} })
	conn.close()

	select {
	case <-lateNotified:
	case <-time.After(2 * time.Second):
		t.Fatal("callback registered after the bulk unregister never ran on close")
	}
	if got := fired.Load(); got != 0 {
		t.Fatalf("unregistered callbacks fired = %d, want 0", got)
	}
}

func TestHandleSubscribeDeliversOnlyToCallingConnection(t *testing.T) {
	ts := startServer(t, testConfig())
	var captured RunEventSubscriber
	ts.HandleSubscribe("v1/subscribeRun", func(sub RunEventSubscriber, params []byte) (any, *proto.Error) {
		captured = sub
		return map[string]any{"status": "running"}, nil
	})

	subscriber := dialClient(t, ts)
	if response := subscriber.initialize(); response.Err != nil {
		t.Fatalf("initialize subscriber: %+v", response.Err)
	}
	bystander := dialClient(t, ts)
	if response := bystander.initialize(); response.Err != nil {
		t.Fatalf("initialize bystander: %+v", response.Err)
	}

	response := subscriber.call("sub-1", "v1/subscribeRun", map[string]any{"runId": "r-1"})
	if response.Err != nil {
		t.Fatalf("subscribeRun: %+v", response.Err)
	}
	if captured == nil {
		t.Fatal("subscribe handler did not receive a RunEventSubscriber")
	}

	captured.DeliverRunEvent(proto.RunEventPayload{RunID: "r-1", Sequence: 0, Type: "run_created"})
	method, params := subscriber.readNotification(t)
	if method != "v1/runEvent" {
		t.Fatalf("method = %s", method)
	}
	var event proto.RunEventPayload
	if err := json.Unmarshal(params, &event); err != nil {
		t.Fatal(err)
	}
	if event.RunID != "r-1" || event.Type != "run_created" {
		t.Fatalf("event = %+v", event)
	}

	captured.DeliverRunGap("r-1", 3)
	method, params = subscriber.readNotification(t)
	if method != "v1/nodeEvent" {
		t.Fatalf("method = %s", method)
	}
	var gap proto.NodeEvent
	if err := json.Unmarshal(params, &gap); err != nil {
		t.Fatal(err)
	}
	if gap.Kind != proto.NodeEventRunEventGap {
		t.Fatalf("kind = %s", gap.Kind)
	}

	// The bystander must observe neither notification: assert the next
	// frame it reads is its own ping response.
	pingResponse := bystander.call("quiet", "v1/ping", map[string]any{})
	if pingResponse.Err != nil {
		t.Fatalf("ping: %+v", pingResponse.Err)
	}
}

func TestHandleWithClientPassesHandshakeClientName(t *testing.T) {
	ts := startServer(t, testConfig())
	var captured string
	ts.HandleWithClient("v1/whoami", func(clientName string, params []byte) (any, *proto.Error) {
		captured = clientName
		return map[string]any{}, nil
	})

	client := dialClient(t, ts)
	if response := client.call("init-1", "v1/initialize", map[string]any{
		"token":  testToken,
		"client": map[string]any{"name": "doppels-cli", "version": "0.0.1"},
	}); response.Err != nil {
		t.Fatalf("initialize: %+v", response.Err)
	}
	if response := client.call("w1", "v1/whoami", map[string]any{}); response.Err != nil {
		t.Fatalf("whoami: %+v", response.Err)
	}
	if captured != "doppels-cli" {
		t.Fatalf("captured client name = %q, want doppels-cli", captured)
	}
}

func TestServerClosesConnectionWithoutHandshake(t *testing.T) {
	cfg := testConfig()
	cfg.HandshakeTimeout = 150 * time.Millisecond
	ts := startServer(t, cfg)

	conn, err := transport.Unix{}.Dial(ts.path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	buf := make([]byte, 1)
	for {
		conn.SetReadDeadline(deadline)
		if _, err := conn.Read(buf); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatal("server never closed the unauthed connection")
			}
			if errors.Is(err, io.EOF) {
				return // server closed: pass
			}
			t.Fatalf("read: %v", err)
		}
	}
}

func TestServerShutdownAcksThenStopsServing(t *testing.T) {
	cfg := testConfig()
	ts := startServer(t, cfg)
	client := dialClient(t, ts)
	if response := client.initialize(); response.Err != nil {
		t.Fatalf("initialize: %+v", response.Err)
	}

	response := client.call("sd", "v1/shutdown", map[string]any{"reason": "restart"})
	if response.Err != nil {
		t.Fatalf("shutdown: %+v", response.Err)
	}

	// Connection closes after the ack.
	deadline := time.Now().Add(3 * time.Second)
	client.conn.SetReadDeadline(deadline)
	if _, err := client.decoder.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after shutdown, got %v", err)
	}

	// Serve loop exits.
	select {
	case err := <-ts.done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop serving after shutdown")
	}

	// New connections are refused (listener closed).
	if _, err := (transport.Unix{}).Dial(ts.path); err == nil {
		t.Fatal("listener must be closed after shutdown")
	}
}

func TestServerServesMultipleClients(t *testing.T) {
	ts := startServer(t, testConfig())
	a := dialClient(t, ts)
	b := dialClient(t, ts)
	if response := a.initialize(); response.Err != nil {
		t.Fatalf("a initialize: %+v", response.Err)
	}
	if response := b.initialize(); response.Err != nil {
		t.Fatalf("b initialize: %+v", response.Err)
	}

	// Interleaved calls on both connections stay correctly matched.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			if response := a.call(fmt.Sprintf("a-%d", i), "v1/ping", (map[string]any{})); response.Err != nil {
				t.Errorf("a ping: %+v", response.Err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			if response := b.call(fmt.Sprintf("b-%d", i), "v1/ping", (map[string]any{})); response.Err != nil {
				t.Errorf("b ping: %+v", response.Err)
				return
			}
		}
	}()
	wg.Wait()
}

func TestServerMalformedAndBatchFrames(t *testing.T) {
	ts := startServer(t, testConfig())
	client := dialClient(t, ts)

	// Malformed JSON → -32700. Write raw bytes: encoder.WriteFrame would
	// validate the json.RawMessage before writing.
	if _, err := client.conn.Write([]byte(`{"jsonrpc":"2.0"` + "\n")); err != nil {
		t.Fatal(err)
	}
	response := client.readResponse()
	if response.Err == nil || response.Err.Code != proto.CodeParse {
		t.Fatalf("err = %+v, want -32700", response.Err)
	}

	// Batch → -32600.
	if _, err := client.conn.Write([]byte(`[{"jsonrpc":"2.0","id":1,"method":"v1/ping"}]` + "\n")); err != nil {
		t.Fatal(err)
	}
	response = client.readResponse()
	if response.Err == nil || response.Err.Code != proto.CodeInvalidRequest {
		t.Fatalf("err = %+v, want -32600", response.Err)
	}

	// Connection survives protocol errors once initialized.
	if response := client.initialize(); response.Err != nil {
		t.Fatalf("initialize after errors: %+v", response.Err)
	}
}

func decodeResult(t *testing.T, response *proto.Response, out any) {
	t.Helper()
	if response.Err != nil {
		t.Fatalf("unexpected error: %+v", response.Err)
	}
	raw, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode result %s: %v", raw, err)
	}
}

// TestServerClosesSlowClientOnRequiredQueueSaturation pins the RFC §15
// policy: if the outbound queue cannot deliver an RPC response, the
// client is hanging on a request that will never arrive, so the server
// closes the connection rather than dropping silently.
//
// We simulate a stuck client by closing the read side of the socket and
// then writing enough RPC requests to fill both the OS TCP recv buffer and
// the in-process outbound channel (1024 entries). Once the writer blocks,
// new RPC enqueues fall into the default branch and close the connection.
func TestServerClosesSlowClientOnRequiredQueueSaturation(t *testing.T) {
	ts := startServer(t, testConfig())
	conn, err := (transport.Unix{}).Dial(ts.path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := (proto.NewEncoder(conn)).WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": "init", "method": "v1/initialize",
		"params": map[string]any{
			"token":  testToken,
			"client": map[string]any{"name": "slow", "version": "0"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := proto.NewDecoder(conn).ReadFrame(); err != nil {
		t.Fatal(err)
	}

	// Half-close the read side: the server can still send but every write
	// once the kernel buffers fill will block. Combine with enough RPCs to
	// also fill the in-process outbound queue.
	if tcpConn, ok := conn.(*net.UnixConn); ok {
		if err := tcpConn.CloseRead(); err != nil {
			t.Fatal(err)
		}
	}

	// Fire 1500 RPCs without ever reading. With CloseRead active, the
	// server-side write loop must block once its TCP send buffer (default
	// 256 KiB on Linux) fills; outbound queue (1024) overflows next.
	request := []byte(`{"jsonrpc":"2.0","id":"sat","method":"v1/getNodeStatus","params":{}}` + "\n")
	for i := 0; i < 1500; i++ {
		if _, err := conn.Write(request); err != nil {
			break
		}
	}

	// Reading any byte means the server kept the connection open. RFC §15
	// requires the client to detect a framing issue via connection close.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected server to close saturated connection")
	}
}

// TestServerDropsExcessFramesForBestEffortNotifications pins that a slow
// subscriber can drop notifications without losing protocol state. The
// connection survives (only RPC responses force a close on saturation).
func TestServerDropsExcessNotificationsWithoutClosing(t *testing.T) {
	ts := startServer(t, testConfig())
	conn, err := (transport.Unix{}).Dial(ts.path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	enc := proto.NewEncoder(conn)
	decoder := proto.NewDecoder(conn)
	if err := enc.WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": "init", "method": "v1/initialize",
		"params": map[string]any{
			"token":  testToken,
			"client": map[string]any{"name": "x", "version": "0"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":"sub","method":"v1/subscribeNode","params":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.ReadFrame(); err != nil {
		t.Fatal(err)
	}

	// Hold off reading; flood with notifications.
	for i := 0; i < 2000; i++ {
		ts.EmitNodeEvent(proto.NodeEvent{Kind: proto.NodeEventRunStarted, Payload: map[string]any{"i": i}})
	}

	// Drain one response (or notification) to confirm the connection is
	// still usable after the flood. Reading just one frame is enough to
	// prove the server has not torn the connection down for notifications.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := decoder.ReadFrame(); err != nil {
		t.Fatalf("expected at least one frame after notification flood; got %v", err)
	}
}

// failingWriter fails every write, standing in for a client that vanished
// between the dispatch and the flush of its response.
type failingWriter struct {
	net.Conn
}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("peer gone") }

// TestShutdownProceedsWhenAckCannotBeWritten pins that a client disappearing
// mid-shutdown cannot strand the Runner: the ack is best-effort evidence for
// the client, the shutdown itself is not conditional on it. Without this the
// server stays in `draining` forever, refusing every new operation while
// never running OnShutdown.
func TestShutdownProceedsWhenAckCannotBeWritten(t *testing.T) {
	var shutdowns atomic.Int32
	config := testConfig()
	config.OnShutdown = func() { shutdowns.Add(1) }
	server := New(config)
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	conn := newConnection(server, failingWriter{local})
	message, protoErr := proto.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"v1/shutdown"}`))
	if protoErr != nil {
		t.Fatal(protoErr)
	}
	server.dispatch(conn, message)
	done := make(chan struct{})
	go conn.writeLoop(done)
	<-done

	select {
	case <-server.closed:
	case <-time.After(time.Second):
		t.Fatalf("server stranded draining after a failed ack (OnShutdown calls = %d)", shutdowns.Load())
	}
	if calls := shutdowns.Load(); calls != 1 {
		t.Fatalf("OnShutdown ran %d times, want exactly 1", calls)
	}
	server.Close()
}

// TestSubscribeNodeDoesNotDropEventsDuringSnapshot pins the other half of the
// subscribe ordering contract. Delivering the snapshot first is only correct
// if nothing is lost in between: an event raised while the snapshot is being
// computed must still reach the subscriber, after the response.
func TestSubscribeNodeDoesNotDropEventsDuringSnapshot(t *testing.T) {
	var server *testServer
	var snapshots atomic.Int32
	config := testConfig()
	config.NodeStatus = func() proto.NodeStatus {
		// The second snapshot is the subscribeNode one (the first belongs to
		// initialize): raise an event while the handler is still inside it.
		if snapshots.Add(1) == 2 {
			server.EmitNodeEvent(proto.NodeEvent{Kind: "workspaceAdded", Payload: map[string]any{"root": "/tmp/late"}})
		}
		return proto.NodeStatus{State: "online", Workspaces: []proto.WorkspaceSummary{}}
	}
	server = startServer(t, config)
	client := dialClient(t, server)
	client.initialize()

	if response := client.call(2, "v1/subscribeNode", nil); response.Err != nil {
		t.Fatalf("subscribeNode: %+v", response.Err)
	}
	if err := client.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	method, params := client.readNotification(t)
	if method != "v1/nodeEvent" {
		t.Fatalf("method = %s, want v1/nodeEvent", method)
	}
	if !bytes.Contains(params, []byte("/tmp/late")) {
		t.Fatalf("event lost around the snapshot: %s", params)
	}
}

// TestServeWaitsForInFlightHandlers pins that Serve returning means "no
// Step-touching work is still running". A supervisor that sees the process
// exit while a handler mutates state on disk gets corruption, not shutdown.
func TestServeWaitsForInFlightHandlers(t *testing.T) {
	server := startServer(t, testConfig())
	entered := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	server.Handle("v1/block", func([]byte) (any, *proto.Error) {
		close(entered)
		<-release
		finished.Store(true)
		return map[string]any{}, nil
	})
	client := dialClient(t, server)
	client.initialize()
	if err := client.encoder.WriteFrame(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "v1/block"}); err != nil {
		t.Fatal(err)
	}
	<-entered

	go server.Close()
	select {
	case <-server.done:
		if !finished.Load() {
			t.Fatal("Serve returned while a handler was still running")
		}
	case <-time.After(200 * time.Millisecond):
		// Serve is correctly blocked on the handler; let it finish.
	}
	close(release)
	select {
	case <-server.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve never returned after the handler finished")
	}
	if !finished.Load() {
		t.Fatal("handler never completed")
	}
}

// TestClosedConnectionStopsExecutingBufferedRequests pins that once a
// connection is closed, requests already sitting in the read buffer are not
// executed: a client pipelining `shutdown` plus a Run request must not have
// the Run started by a connection that is already gone.
func TestClosedConnectionStopsExecutingBufferedRequests(t *testing.T) {
	server := New(testConfig())
	local, remote := net.Pipe()
	defer remote.Close()
	conn := newConnection(server, local)
	conn.initialize("test")
	var executed atomic.Int32
	server.Handle("v1/closeme", func([]byte) (any, *proto.Error) {
		conn.close()
		return map[string]any{}, nil
	})
	server.Handle("v1/effect", func([]byte) (any, *proto.Error) {
		executed.Add(1)
		return map[string]any{}, nil
	})
	done := make(chan struct{})
	go func() {
		conn.serve()
		close(done)
	}()
	go func() {
		_, _ = remote.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"v1/closeme\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"v1/effect\"}\n"))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serve never returned after the connection closed")
	}
	if count := executed.Load(); count != 0 {
		t.Fatalf("buffered request executed %d times after close", count)
	}
}

// TestConnectionStateIsRaceFree pins that the diagnostic client name is read
// under the same lock that initialize writes it: the overflow log path runs
// on the emitting goroutine while the handshake runs on the reader.
func TestConnectionStateIsRaceFree(t *testing.T) {
	server := New(testConfig())
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	conn := newConnection(server, local)
	conn.beginNodeSubscription()
	conn.flushNodeSubscription()
	for i := 0; i < outboundBufferSize; i++ {
		conn.enqueue(proto.NewNotification("v1/nodeEvent", nil), false)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			conn.initialize("client")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			conn.sendNodeEvent(proto.NodeEvent{Kind: "workspaceAdded"})
		}
	}()
	wg.Wait()
}
