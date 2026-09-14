package runs

import (
	"encoding/json"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/server"
)

// RPCServer is the subset of server.Server used to expose the v1 Run method
// surface (RFC §9): startRun, cancelRun, getRun, listRuns, getRunLogs, and
// subscribeRun.
type RPCServer interface {
	Handle(method string, handler server.Handler)
	HandleWithClient(method string, handler server.ClientHandler)
	HandleSubscribe(method string, handler server.SubscribeHandler)
}

// RegisterRPC attaches the PR6 method surface to the Runner server.
func RegisterRPC(target RPCServer, manager *Manager) {
	target.HandleWithClient("v1/startRun", func(clientName string, params []byte) (any, *proto.Error) {
		return manager.Start(clientName, params)
	})

	target.Handle("v1/cancelRun", func(params []byte) (any, *proto.Error) {
		var request struct {
			RunID  string `json:"runId"`
			Reason string `json:"reason"`
		}
		if err := decodeRPCParams(params, &request); err != nil {
			return nil, err
		}
		if request.RunID == "" {
			return nil, invalidParams("runId is required")
		}
		status, err := manager.Cancel(request.RunID, request.Reason)
		if err != nil {
			return nil, err
		}
		return map[string]string{"status": status}, nil
	})

	target.Handle("v1/getRun", func(params []byte) (any, *proto.Error) {
		var request struct {
			RunID         string `json:"runId"`
			IncludeEvents bool   `json:"includeEvents"`
		}
		if err := decodeRPCParams(params, &request); err != nil {
			return nil, err
		}
		if request.RunID == "" {
			return nil, invalidParams("runId is required")
		}
		return manager.GetRun(request.RunID, request.IncludeEvents)
	})

	target.Handle("v1/listRuns", func(params []byte) (any, *proto.Error) {
		var request struct {
			Workspace  string `json:"workspace"`
			Capability string `json:"capability"`
			Status     string `json:"status"`
			Limit      int    `json:"limit"`
			Cursor     string `json:"cursor"`
		}
		if err := decodeRPCParams(params, &request); err != nil {
			return nil, err
		}
		return manager.ListRuns(ListParams{
			Workspace: request.Workspace, Capability: request.Capability,
			Status: request.Status, Limit: request.Limit, Cursor: request.Cursor,
		})
	})

	target.Handle("v1/getRunLogs", func(params []byte) (any, *proto.Error) {
		var request struct {
			RunID  string `json:"runId"`
			StepID string `json:"stepId"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := decodeRPCParams(params, &request); err != nil {
			return nil, err
		}
		if request.RunID == "" {
			return nil, invalidParams("runId is required")
		}
		return manager.GetRunLogs(LogsParams{
			RunID: request.RunID, StepID: request.StepID,
			Offset: request.Offset, Limit: request.Limit,
		})
	})

	target.HandleSubscribe("v1/subscribeRun", func(sub server.RunEventSubscriber, params []byte) (any, *proto.Error) {
		var request struct {
			RunID        string `json:"runId"`
			FromSequence int    `json:"fromSequence"`
		}
		if err := decodeRPCParams(params, &request); err != nil {
			return nil, err
		}
		if request.RunID == "" {
			return nil, invalidParams("runId is required")
		}
		return manager.Subscribe(request.RunID, request.FromSequence, sub)
	})
}

func decodeRPCParams(params []byte, out any) *proto.Error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, out); err != nil {
		return invalidParams(err.Error())
	}
	return nil
}
