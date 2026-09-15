package runs

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runner/proto"
)

type approvalDecision struct {
	approved bool
}

// PendingApproval is an in-memory HITL wait exposed by
// v1/listPendingApprovals. It is intentionally not persisted: the execution
// goroutine that can consume a decision does not survive a Runner restart.
type PendingApproval struct {
	RunID       string    `json:"runId"`
	StepID      string    `json:"stepId"`
	Name        string    `json:"name"`
	RequestedAt time.Time `json:"requestedAt"`
}

func approvalKey(runID, stepID string) string {
	return runID + ":" + stepID
}

func (m *Manager) awaitApproval(ctx context.Context, request execution.ApprovalRequest) (bool, error) {
	key := approvalKey(request.RunID, request.StepID)
	decision := make(chan approvalDecision, 1)
	pending := PendingApproval{
		RunID: request.RunID, StepID: request.StepID, Name: request.Name,
		RequestedAt: request.RequestedAt,
	}

	m.pendingApprovalsMu.Lock()
	m.pendingApprovals[key] = decision
	m.pendingApprovalInfo[key] = pending
	m.pendingApprovalsMu.Unlock()

	if m.config.EmitNodeEvent != nil {
		m.config.EmitNodeEvent(proto.NodeEvent{Kind: proto.NodeEventApprovalPending, Payload: pending})
	}

	defer func() {
		m.pendingApprovalsMu.Lock()
		if m.pendingApprovals[key] == decision {
			delete(m.pendingApprovals, key)
			delete(m.pendingApprovalInfo, key)
		}
		m.pendingApprovalsMu.Unlock()
	}()

	select {
	case result := <-decision:
		return result.approved, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// ListPendingApprovals returns a point-in-time snapshot ordered by the time
// the engine began waiting. Equal timestamps use stable identifiers only to
// keep repeated snapshots deterministic.
func (m *Manager) ListPendingApprovals() []PendingApproval {
	m.pendingApprovalsMu.Lock()
	result := make([]PendingApproval, 0, len(m.pendingApprovalInfo))
	for _, pending := range m.pendingApprovalInfo {
		result = append(result, pending)
	}
	m.pendingApprovalsMu.Unlock()
	sort.Slice(result, func(i, j int) bool {
		if result[i].RequestedAt.Equal(result[j].RequestedAt) {
			if result[i].RunID == result[j].RunID {
				return result[i].StepID < result[j].StepID
			}
			return result[i].RunID < result[j].RunID
		}
		return result[i].RequestedAt.Before(result[j].RequestedAt)
	})
	return result
}

// DecideApproval atomically claims a pending decision and wakes exactly one
// engine goroutine. A second decision observes the entry as already gone and
// receives approvalNotFound instead of racing a send or closing a channel.
func (m *Manager) DecideApproval(runID, stepID, decision string) *proto.Error {
	if decision != "approve" && decision != "reject" {
		return invalidParams("decision must be approve or reject")
	}
	key := approvalKey(runID, stepID)
	m.pendingApprovalsMu.Lock()
	waiter := m.pendingApprovals[key]
	if waiter != nil {
		delete(m.pendingApprovals, key)
		delete(m.pendingApprovalInfo, key)
	}
	m.pendingApprovalsMu.Unlock()
	if waiter != nil {
		waiter <- approvalDecision{approved: decision == "approve"}
		return nil
	}

	if !safeRunID(runID) {
		return &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	_, _, _, err := m.findRecord(runID)
	if errors.Is(err, sql.ErrNoRows) {
		return &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	if err != nil {
		return internalError(err)
	}
	return &proto.Error{Code: proto.CodeApprovalNotFound, Message: "Approval not found"}
}

func (m *Manager) installNodeEventEmitter(emit func(proto.NodeEvent)) {
	if m.config.EmitNodeEvent == nil {
		m.config.EmitNodeEvent = emit
	}
}
