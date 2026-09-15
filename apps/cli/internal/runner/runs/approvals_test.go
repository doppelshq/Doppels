package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runstate"
)

func TestManagerListPendingApprovalsReturnsEmptyByDefault(t *testing.T) {
	service, _ := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	if got := manager.ListPendingApprovals(); len(got) != 0 {
		t.Fatalf("pending approvals = %#v, want empty", got)
	}
}

func TestManagerDecideApprovalWakesRun(t *testing.T) {
	manager, root, started := startPendingApproval(t, "approve")
	defer manager.Close()

	if manager.Active() != 1 {
		t.Fatalf("active runs = %d, want 1 while approval is pending", manager.Active())
	}
	if rpcErr := manager.DecideApproval(started.RunID, "run", "approve"); rpcErr != nil {
		t.Fatalf("DecideApproval: %+v", rpcErr)
	}
	waitForStatus(t, root, started.RunID, "succeeded")
}

func TestManagerDecideApprovalRejectsRun(t *testing.T) {
	manager, root, started := startPendingApproval(t, "reject")
	defer manager.Close()

	if rpcErr := manager.DecideApproval(started.RunID, "run", "reject"); rpcErr != nil {
		t.Fatalf("DecideApproval: %+v", rpcErr)
	}
	waitForStatus(t, root, started.RunID, "cancelled")
	detail, err := runstate.Load(root, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Events) < 2 {
		t.Fatalf("events = %#v", detail.Events)
	}
	got := []string{detail.Events[len(detail.Events)-2].Type, detail.Events[len(detail.Events)-1].Type}
	if got[0] != "approval_rejected" || got[1] != "run_cancelled" {
		t.Fatalf("terminal events = %v, want [approval_rejected run_cancelled]", got)
	}
}

func TestManagerDecideApprovalReturnsApprovalNotFound(t *testing.T) {
	manager, _, started := startPendingApproval(t, "missing-approval")
	defer manager.Close()

	if rpcErr := manager.DecideApproval(started.RunID, "another-step", "approve"); rpcErr == nil || rpcErr.Code != proto.CodeApprovalNotFound {
		t.Fatalf("error = %+v, want -32007", rpcErr)
	}
}

func TestManagerDecideApprovalReturnsRunNotFound(t *testing.T) {
	service, _ := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	if rpcErr := manager.DecideApproval("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "run", "approve"); rpcErr == nil || rpcErr.Code != proto.CodeRunNotFound {
		t.Fatalf("error = %+v, want -32006", rpcErr)
	}
}

func TestManagerEmitsApprovalPendingNodeEvent(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	writeApprovalRecipe(t, root)
	events := make(chan proto.NodeEvent, 1)
	manager := NewManager(context.Background(), service, Config{
		NodeID:      "node-test",
		Environment: []string{"PATH=" + os.Getenv("PATH")},
		EmitNodeEvent: func(event proto.NodeEvent) {
			events <- event
		},
	})
	defer manager.Close()

	started := startApprovalRun(t, manager, root, "node-event")
	select {
	case event := <-events:
		if event.Kind != proto.NodeEventApprovalPending {
			t.Fatalf("kind = %q, want approvalPending", event.Kind)
		}
		payload, ok := event.Payload.(PendingApproval)
		if !ok {
			t.Fatalf("payload = %#v (%T), want PendingApproval", event.Payload, event.Payload)
		}
		if payload.RunID != started.RunID || payload.StepID != "run" || payload.Name != "Run" || payload.RequestedAt.IsZero() {
			t.Fatalf("payload = %#v", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for approvalPending nodeEvent")
	}
}

func TestApprovalRequestedTimestampsMatchPersistedEvent(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	writeApprovalRecipe(t, root)
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var nowMu sync.Mutex
	now := base
	events := make(chan proto.NodeEvent, 1)
	manager := NewManager(context.Background(), service, Config{
		NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")},
		Now: func() time.Time {
			nowMu.Lock()
			defer nowMu.Unlock()
			now = now.Add(time.Millisecond)
			return now
		},
		EmitNodeEvent: func(event proto.NodeEvent) { events <- event },
	})
	defer manager.Close()

	started := startApprovalRun(t, manager, root, "matching-requested-at")
	pending := waitForPendingApproval(t, manager, started.RunID)
	var nodePending PendingApproval
	select {
	case event := <-events:
		var ok bool
		nodePending, ok = event.Payload.(PendingApproval)
		if event.Kind != proto.NodeEventApprovalPending || !ok {
			t.Fatalf("node event = %#v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for approvalPending nodeEvent")
	}
	detail, err := runstate.Load(root, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var requestedAt time.Time
	for _, event := range detail.Events {
		if event.Type == "approval_requested" {
			requestedAt = event.OccurredAt
			break
		}
	}
	if requestedAt.IsZero() {
		t.Fatal("approval_requested event not persisted")
	}

	durableJSON, _ := json.Marshal(requestedAt)
	nodeJSON, _ := json.Marshal(nodePending.RequestedAt)
	listJSON, _ := json.Marshal(pending.RequestedAt)
	if !bytes.Equal(durableJSON, nodeJSON) || !bytes.Equal(durableJSON, listJSON) {
		t.Fatalf("requestedAt bytes differ: durable=%s node=%s list=%s", durableJSON, nodeJSON, listJSON)
	}
}

func TestManagerListsPendingApprovalsInOrder(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	writeApprovalRecipe(t, root)
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	var nowMu sync.Mutex
	now := base
	manager := NewManager(context.Background(), service, Config{
		NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")},
		Now: func() time.Time {
			nowMu.Lock()
			defer nowMu.Unlock()
			now = now.Add(time.Millisecond)
			return now
		},
	})
	defer manager.Close()

	first := startApprovalRun(t, manager, root, "ordered-1")
	waitForPendingApproval(t, manager, first.RunID)
	second := startApprovalRun(t, manager, root, "ordered-2")
	waitForPendingApproval(t, manager, second.RunID)

	got := manager.ListPendingApprovals()
	if len(got) != 2 {
		t.Fatalf("pending approvals = %#v, want 2", got)
	}
	if got[0].RequestedAt.After(got[1].RequestedAt) {
		t.Fatalf("approvals not sorted by requestedAt: %#v", got)
	}
	if got[0].RunID != first.RunID || got[1].RunID != second.RunID {
		t.Fatalf("run order = [%s %s], want [%s %s]", got[0].RunID, got[1].RunID, first.RunID, second.RunID)
	}
}

func startPendingApproval(t *testing.T, key string) (*Manager, string, StartResult) {
	t.Helper()
	service, root := runnerWorkspace(t, true)
	writeApprovalRecipe(t, root)
	manager := NewManager(context.Background(), service, Config{
		NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")},
	})
	started := startApprovalRun(t, manager, root, key)
	waitForPendingApproval(t, manager, started.RunID)
	return manager, root, started
}

func startApprovalRun(t *testing.T, manager *Manager, root, key string) StartResult {
	t.Helper()
	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"interactive","idempotencyKey":` + quote(key) + `}`)
	started, rpcErr := manager.Start("desktop", params)
	if rpcErr != nil {
		t.Fatalf("Start: %+v", rpcErr)
	}
	return started
}

func waitForPendingApproval(t *testing.T, manager *Manager, runID string) PendingApproval {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, approval := range manager.ListPendingApprovals() {
			if approval.RunID == runID {
				return approval
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pending approval for Run %s", runID)
	return PendingApproval{}
}

func writeApprovalRecipe(t *testing.T, root string) {
	t.Helper()
	writeRunFixture(t, filepath.Join(root, ".doppels", "recipes", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: greet-shell, version: 1.0.0}
provides: [greet]
runtime: shell
defaults: {approval: required}
steps:
  - id: run
    name: Run
    run: {shell: sh, script: "export VALUE=approved"}
    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)
}
