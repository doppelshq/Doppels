package runs

import (
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

func TestListPendingApprovalsOverRealSocket(t *testing.T) {
	manager, root, client := startApprovalIntegrationServer(t)
	started := startApprovalOverSocket(t, client, root, "socket-list")

	var approvals []PendingApproval
	deadline := time.Now().Add(5 * time.Second)
	for len(approvals) == 0 && time.Now().Before(deadline) {
		response := client.call("list", "v1/listPendingApprovals", map[string]any{})
		if response.Err != nil {
			t.Fatalf("listPendingApprovals: %+v", response.Err)
		}
		if err := json.Unmarshal(rawResult(t, response), &approvals); err != nil {
			t.Fatal(err)
		}
		if len(approvals) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if len(approvals) != 1 || approvals[0].RunID != started.RunID || approvals[0].StepID != "run" || approvals[0].Name != "Run" || approvals[0].RequestedAt.IsZero() {
		t.Fatalf("approvals = %#v", approvals)
	}
	assertSocketPing(t, client, "list-ping")
	_ = manager
}

func TestDecideApprovalUnblocksRunOverRealSocket(t *testing.T) {
	_, root, client := startApprovalIntegrationServer(t)
	started := startApprovalOverSocket(t, client, root, "socket-decide")
	waitForApprovalOverSocket(t, client, started.RunID)

	response := client.call("decide", "v1/decideApproval", map[string]any{
		"runId": started.RunID, "stepId": "run", "decision": "approve",
	})
	if response.Err != nil {
		t.Fatalf("decideApproval: %+v", response.Err)
	}
	subResponse := client.call("sub", "v1/subscribeRun", map[string]any{"runId": started.RunID})
	if subResponse.Err != nil {
		t.Fatalf("subscribeRun: %+v", subResponse.Err)
	}
	var snapshot SubscribeResult
	if err := json.Unmarshal(rawResult(t, subResponse), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "succeeded" {
		completed := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			method, params := client.readNotification(t)
			if method != "v1/runEvent" {
				continue
			}
			var event proto.RunEventPayload
			if err := json.Unmarshal(params, &event); err != nil {
				t.Fatal(err)
			}
			if event.RunID == started.RunID && event.Type == "run_succeeded" {
				completed = true
				break
			}
		}
		if !completed {
			t.Fatal("Run never completed after approval")
		}
	}
	assertSocketPing(t, client, "decide-ping")
}

func TestServerCloseCancelsPendingApproval(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	writeApprovalRecipe(t, root)
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
	client := dialIntegrationClient(t, socketPath)
	started := startApprovalOverSocket(t, client, root, "server-close")
	waitForApprovalOverSocket(t, client, started.RunID)

	srv.Close()
	waitForStatus(t, root, started.RunID, "interrupted")
	if got := manager.ListPendingApprovals(); len(got) != 0 {
		t.Fatalf("pending approvals after Server.Close = %#v", got)
	}
}

func startApprovalIntegrationServer(t *testing.T) (*Manager, string, *integrationClient) {
	t.Helper()
	service, root := runnerWorkspace(t, true)
	writeApprovalRecipe(t, root)
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

func startApprovalOverSocket(t *testing.T, client *integrationClient, root, key string) StartResult {
	t.Helper()
	response := client.call("start-"+key, "v1/startRun", map[string]any{
		"workspace": root, "capability": "greet", "inputs": map[string]any{"count": 1},
		"approvalMode": "interactive", "idempotencyKey": key,
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

func waitForApprovalOverSocket(t *testing.T, client *integrationClient, runID string) PendingApproval {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := client.call("list-wait", "v1/listPendingApprovals", map[string]any{})
		if response.Err != nil {
			t.Fatalf("listPendingApprovals: %+v", response.Err)
		}
		var approvals []PendingApproval
		if err := json.Unmarshal(rawResult(t, response), &approvals); err != nil {
			t.Fatal(err)
		}
		for _, approval := range approvals {
			if approval.RunID == runID {
				return approval
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for approval for Run %s", runID)
	return PendingApproval{}
}
