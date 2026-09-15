package runs

import (
	"context"
	"os"
	"testing"

	"doppels.so/cli/internal/runner/proto"
)

func TestGetRunReturnsSummaryRequestAndOptionalEvents(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"get"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForStatus(t, root, started.RunID, "succeeded")

	result, rpcErr := manager.GetRun(started.RunID, false)
	if rpcErr != nil {
		t.Fatalf("GetRun: %+v", rpcErr)
	}
	if result.Summary.RunID != started.RunID || result.Summary.Status != "succeeded" || result.Summary.Workspace != root {
		t.Fatalf("summary = %#v", result.Summary)
	}
	if result.Request.ID != started.RequestID {
		t.Fatalf("request = %#v", result.Request)
	}
	if result.Events != nil {
		t.Fatalf("events should be omitted, got %#v", result.Events)
	}

	withEvents, rpcErr := manager.GetRun(started.RunID, true)
	if rpcErr != nil {
		t.Fatalf("GetRun includeEvents: %+v", rpcErr)
	}
	if len(withEvents.Events) == 0 {
		t.Fatal("expected events when includeEvents=true")
	}
	last := withEvents.Events[len(withEvents.Events)-1]
	if last.Type != "run_succeeded" {
		t.Fatalf("last event = %#v", last)
	}
}

// TestGetRunReturnsExactReservedRequestBeforeMaterialization reproduces a
// review finding: startRun durably reserves a Run (index row + exact
// request.json/run.json evidence in the idempotency table) before the engine
// goroutine that writes request.json/run.json to disk ever starts. GetRun
// called in that window used to fail outright (runstate.LoadWithIndex can't
// find files that don't exist yet) even though the caller was just handed a
// valid RunID. testStopAfterReserve makes that window deterministic instead
// of timing-dependent: it returns right after the reservation, before
// request.json/run.json are ever written. GetRun must recover the exact
// reserved Request evidence rather than synthesizing placeholder metadata.
func TestGetRunReturnsExactReservedRequestBeforeMaterialization(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()
	manager.testStopAfterReserve = true

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":7},"approvalMode":"interactive","idempotencyKey":"get-before-materialize"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}

	result, rpcErr := manager.GetRun(started.RunID, true)
	if rpcErr != nil {
		t.Fatalf("GetRun: %+v", rpcErr)
	}
	if result.Summary.RunID != started.RunID || result.Summary.Status != "running" {
		t.Fatalf("summary = %#v", result.Summary)
	}
	if result.Request.ID != started.RequestID {
		t.Fatalf("request.ID = %q, want %q", result.Request.ID, started.RequestID)
	}
	if result.Request.Capability.Name != "greet" {
		t.Fatalf("request.Capability = %#v", result.Request.Capability)
	}
	if got, _ := result.Request.Inputs["count"].(float64); got != 7 {
		t.Fatalf("request.Inputs = %#v, want count=7", result.Request.Inputs)
	}
	if result.Request.IdempotencyKey != "get-before-materialize" {
		t.Fatalf("request.IdempotencyKey = %q", result.Request.IdempotencyKey)
	}
	if len(result.Events) != 0 {
		t.Fatalf("events = %#v, want empty (nothing durable on disk yet)", result.Events)
	}
}

func TestGetRunUnknownReturnsRunNotFound(t *testing.T) {
	service, _ := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	if _, rpcErr := manager.GetRun("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", false); rpcErr == nil || rpcErr.Code != proto.CodeRunNotFound {
		t.Fatalf("GetRun unknown = %+v, want -32006", rpcErr)
	}
}
