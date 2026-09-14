package runs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"doppels.so/cli/internal/runner/proto"
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
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > proto.MaxFrameBytes {
		t.Fatalf("encoded response = %d bytes, want <= %d (proto.MaxFrameBytes)", len(encoded), proto.MaxFrameBytes)
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
