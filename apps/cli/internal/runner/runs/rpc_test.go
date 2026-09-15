package runs

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
)

type fakeRPCServer struct {
	handlers     map[string]server.Handler
	client       map[string]server.ClientHandler
	subscribers  map[string]server.SubscribeHandler
	logSubs      map[string]server.RunLogSubscribeHandler
	nodeEvents   []proto.NodeEvent
	closeHooks   []func()
	capabilities []string
}

func newFakeRPCServer() *fakeRPCServer {
	return &fakeRPCServer{
		handlers:    map[string]server.Handler{},
		client:      map[string]server.ClientHandler{},
		subscribers: map[string]server.SubscribeHandler{},
		logSubs:     map[string]server.RunLogSubscribeHandler{},
	}
}

func (f *fakeRPCServer) Handle(method string, handler server.Handler) { f.handlers[method] = handler }
func (f *fakeRPCServer) HandleWithClient(method string, handler server.ClientHandler) {
	f.client[method] = handler
}
func (f *fakeRPCServer) HandleSubscribe(method string, handler server.SubscribeHandler) {
	f.subscribers[method] = handler
}
func (f *fakeRPCServer) HandleRunLogSubscribe(method string, handler server.RunLogSubscribeHandler) {
	f.logSubs[method] = handler
}
func (f *fakeRPCServer) EmitNodeEvent(event proto.NodeEvent) {
	f.nodeEvents = append(f.nodeEvents, event)
}
func (f *fakeRPCServer) OnClose(fn func()) { f.closeHooks = append(f.closeHooks, fn) }
func (f *fakeRPCServer) EnableCapability(capability string) {
	f.capabilities = append(f.capabilities, capability)
}

func rpcParams(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRegisterRPCStartCancelGetListLogsAndSubscribe(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	target := newFakeRPCServer()
	RegisterRPC(target, manager)
	if target.handlers["v1/listPendingApprovals"] == nil || target.handlers["v1/decideApproval"] == nil {
		t.Fatal("approval RPC handlers were not registered")
	}
	if target.logSubs["v1/subscribeRunLogs"] == nil {
		t.Fatal("live-log RPC handler was not registered")
	}
	if len(target.capabilities) != 1 || target.capabilities[0] != proto.CapabilityLiveLogs {
		t.Fatalf("capabilities = %#v, want liveLogs", target.capabilities)
	}
	if len(target.closeHooks) != 1 {
		t.Fatalf("close hooks = %d, want 1", len(target.closeHooks))
	}

	startResult, protoErr := target.client["v1/startRun"]("cli", rpcParams(t, map[string]any{
		"workspace": root, "capability": "greet", "inputs": map[string]any{"count": 1},
		"approvalMode": "auto", "idempotencyKey": "rpc-start",
	}))
	if protoErr != nil {
		t.Fatalf("startRun: %+v", protoErr)
	}
	started := startResult.(StartResult)
	waitForStatus(t, root, started.RunID, "succeeded")

	getResult, protoErr := target.handlers["v1/getRun"](rpcParams(t, map[string]any{"runId": started.RunID, "includeEvents": true}))
	if protoErr != nil {
		t.Fatalf("getRun: %+v", protoErr)
	}
	if getResult.(GetRunResult).Summary.Status != "succeeded" {
		t.Fatalf("getRun result = %#v", getResult)
	}

	listResult, protoErr := target.handlers["v1/listRuns"](rpcParams(t, map[string]any{"workspace": root}))
	if protoErr != nil {
		t.Fatalf("listRuns: %+v", protoErr)
	}
	if len(listResult.(ListResult).Runs) != 1 {
		t.Fatalf("listRuns result = %#v", listResult)
	}

	logsResult, protoErr := target.handlers["v1/getRunLogs"](rpcParams(t, map[string]any{"runId": started.RunID}))
	if protoErr != nil {
		t.Fatalf("getRunLogs: %+v", protoErr)
	}
	if len(logsResult.(*LogsResult).Files) == 0 {
		t.Fatalf("getRunLogs result = %#v", logsResult)
	}

	sub := newFakeSubscriber()
	subResult, protoErr := target.subscribers["v1/subscribeRun"](sub, rpcParams(t, map[string]any{"runId": started.RunID}))
	if protoErr != nil {
		t.Fatalf("subscribeRun: %+v", protoErr)
	}
	if subResult.(SubscribeResult).Status != "succeeded" {
		t.Fatalf("subscribeRun result = %#v", subResult)
	}

	if _, protoErr := target.handlers["v1/cancelRun"](rpcParams(t, map[string]any{"runId": started.RunID})); protoErr != nil {
		t.Fatalf("cancelRun on terminal run should be idempotent: %+v", protoErr)
	}

	if _, protoErr := target.handlers["v1/getRun"](rpcParams(t, map[string]any{})); protoErr == nil || protoErr.Code != proto.CodeInvalidParams {
		t.Fatalf("getRun missing runId error = %+v, want -32602", protoErr)
	}
}
