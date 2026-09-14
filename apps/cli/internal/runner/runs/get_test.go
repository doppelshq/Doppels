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

func TestGetRunUnknownReturnsRunNotFound(t *testing.T) {
	service, _ := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	if _, rpcErr := manager.GetRun("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", false); rpcErr == nil || rpcErr.Code != proto.CodeRunNotFound {
		t.Fatalf("GetRun unknown = %+v, want -32006", rpcErr)
	}
}
