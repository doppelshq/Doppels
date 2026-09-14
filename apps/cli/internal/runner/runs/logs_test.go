package runs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
	"doppels.so/cli/internal/runner/transport"
)

func TestGetRunLogsListsFilesAndOmitsContentWithoutStepID(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	started := startAndFinish(t, manager, root, "logs-1")

	result, rpcErr := manager.GetRunLogs(LogsParams{RunID: started.RunID})
	if rpcErr != nil {
		t.Fatalf("GetRunLogs: %+v", rpcErr)
	}
	if len(result.Files) == 0 {
		t.Fatal("expected at least one log file")
	}
	if result.Content != nil {
		t.Fatal("content must be omitted without a stepId")
	}

	stepID := result.Files[0].StepID
	withStep, rpcErr := manager.GetRunLogs(LogsParams{RunID: started.RunID, StepID: stepID})
	if rpcErr != nil {
		t.Fatalf("GetRunLogs with stepId: %+v", rpcErr)
	}
	if withStep.Content == nil {
		t.Fatal("content must be present with a stepId")
	}
	for _, file := range withStep.Files {
		if file.StepID != stepID {
			t.Fatalf("files leaked another step: %#v", withStep.Files)
		}
	}
}

func TestGetRunLogsUnknownRunReturnsRunNotFound(t *testing.T) {
	service, _ := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	if _, rpcErr := manager.GetRunLogs(LogsParams{RunID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}); rpcErr == nil || rpcErr.Code != proto.CodeRunNotFound {
		t.Fatalf("GetRunLogs unknown = %+v, want -32006", rpcErr)
	}
}

// TestGetRunLogsContentIsByteExactBase64EvenForInvalidUTF8 reproduces review
// finding 8's correctness half: log content is arbitrary subprocess bytes,
// not guaranteed valid UTF-8. Returning it as a plain JSON string would
// silently mangle invalid byte sequences (json.Marshal replaces them with
// U+FFFD) and could split a multi-byte rune exactly at a page boundary.
// Content must be base64 so pages round-trip the exact original bytes
// regardless of what the subprocess wrote.
func TestGetRunLogsContentIsByteExactBase64EvenForInvalidUTF8(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	started := manualRun(t, manager, root, "binary-safe")
	runDir := filepath.Join(root, ".doppels", "runs", started.RunID)
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Invalid UTF-8 (a lone continuation byte) plus a multi-byte rune (é,
	// 2 bytes) positioned so a naive byte-offset page would split it.
	raw := []byte{'a', 'b', 0xC3, 0xA9, 0x80, 'c', 'd'}
	if err := os.WriteFile(filepath.Join(runDir, "logs", "run.stdout.log"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	result, rpcErr := manager.GetRunLogs(LogsParams{RunID: started.RunID, StepID: "run", Offset: 2, Limit: 3})
	if rpcErr != nil {
		t.Fatalf("GetRunLogs: %+v", rpcErr)
	}
	if result.Content == nil {
		t.Fatal("content missing")
	}
	decoded, err := base64.StdEncoding.DecodeString(*result.Content)
	if err != nil {
		t.Fatalf("content is not valid base64: %v", err)
	}
	want := raw[2:5] // {0xC3, 0xA9, 0x80}: the split rune plus the invalid byte
	if string(decoded) != string(want) {
		t.Fatalf("decoded = %#v, want %#v (exact bytes, no UTF-8 mangling)", decoded, want)
	}
}

// TestGetRunLogsResponseNeverExceedsMaxFrameBytes reproduces review finding
// 8's framing half: a raw content window near the RFC's stated "4 MiB"
// cap, once base64-encoded and wrapped in the JSON-RPC envelope alongside
// files[] metadata, can exceed the wire's hard MaxFrameBytes limit
// (proto.MaxFrameBytes) — a frame the transport cannot even write. Even a
// maximal request must yield a response the encoder can actually frame.
func TestGetRunLogsResponseNeverExceedsMaxFrameBytes(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	started := manualRun(t, manager, root, "frame-safe")
	runDir := filepath.Join(root, ".doppels", "runs", started.RunID)
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 6<<20) // bigger than one MaxFrameBytes frame
	for i := range big {
		big[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(runDir, "logs", "run.stdout.log"), big, 0o600); err != nil {
		t.Fatal(err)
	}

	result, rpcErr := manager.GetRunLogs(LogsParams{RunID: started.RunID, StepID: "run", Limit: 1 << 30})
	if rpcErr != nil {
		t.Fatalf("GetRunLogs: %+v", rpcErr)
	}
	if fitErr := result.FitResponseFrame("logs-frame", proto.MaxFrameBytes); fitErr != nil {
		t.Fatalf("FitResponseFrame: %+v", fitErr)
	}
	encoded, err := json.Marshal(proto.NewResponse("logs-frame", &result))
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > proto.MaxFrameBytes {
		t.Fatalf("encoded response = %d bytes, want <= %d (proto.MaxFrameBytes)", len(encoded), proto.MaxFrameBytes)
	}
}

// TestGetRunLogsAdversarialMetadataKeepsSocketUsable reproduces the
// remaining finding-8 framing hole: files[] alone can exceed 4 MiB when a
// Run contains thousands of valid log files, even with no content field.
// The daemon must return an in-frame response and keep the connection alive.
func TestGetRunLogsAdversarialMetadataKeepsSocketUsable(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()
	started := manualRun(t, manager, root, "metadata-frame")
	runDir := filepath.Join(root, ".doppels", "runs", started.RunID)
	const files = 2_000
	// '<' is a valid filename byte on Unix and encoding/json escapes it in
	// both stepId and path. This exceeds 4 MiB with thousands—not tens of
	// thousands—of real filesystem entries, keeping the regression focused.
	prefix := strings.Repeat("<", 220)
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("%s%05d.stdout.log", prefix, i)
		if err := os.WriteFile(filepath.Join(runDir, "logs", name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

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

	if err := client.encoder.WriteFrame(map[string]any{
		"jsonrpc": "2.0", "id": "metadata", "method": "v1/getRunLogs",
		"params": map[string]any{"runId": started.RunID},
	}); err != nil {
		t.Fatal(err)
	}
	frame, err := client.decoder.ReadFrame()
	if err != nil {
		t.Fatalf("getRunLogs closed the connection instead of returning an in-frame response: %v", err)
	}
	if len(frame) > proto.MaxFrameBytes {
		t.Fatalf("getRunLogs frame = %d bytes, max %d", len(frame), proto.MaxFrameBytes)
	}
	var response proto.Response
	if err := json.Unmarshal(frame, &response); err != nil {
		t.Fatal(err)
	}
	if response.Err == nil {
		t.Fatalf("adversarial metadata unexpectedly returned an unbounded success response")
	}
	if response.Err.Code != proto.CodeInvalidParams {
		t.Fatalf("adversarial metadata error = %+v, want invalidParams", response.Err)
	}

	ping := client.call("metadata-ping", "v1/ping", map[string]any{})
	if ping.Err != nil {
		t.Fatalf("connection unusable after bounded logs response: %+v", ping.Err)
	}
}

// manualRun starts a Run against a Capability with no Recipe (durable
// pendingManual) purely as a vehicle to get a real Run directory under
// runID that this package's log tests can drop synthetic log files into.
func manualRun(t *testing.T, manager *Manager, root, key string) StartResult {
	t.Helper()
	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"interactive","idempotencyKey":"` + key + `"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForStatus(t, root, started.RunID, "pending_manual")
	return started
}
