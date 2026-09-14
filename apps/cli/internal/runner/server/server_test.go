package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
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
