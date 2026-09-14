package runs

import (
	"database/sql"
	"errors"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runstate"
)

// GetRunResult is the v1/getRun response: the indexed summary, the durable
// Request record, and (only when requested) the full event log.
type GetRunResult struct {
	Summary proto.RunSummary        `json:"summary"`
	Request execution.RequestRecord `json:"request"`
	Events  []proto.RunEventPayload `json:"events,omitempty"`
}

// GetRun reads the authoritative on-disk Detail (request.json, run.json,
// events.jsonl) enriched with the indexed status for a still-running Run
// (e.g. pendingManual), matching cancelRun/subscribeRun's source of truth.
func (m *Manager) GetRun(runID string, includeEvents bool) (GetRunResult, *proto.Error) {
	if !safeRunID(runID) {
		return GetRunResult{}, &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	root, idx, record, err := m.findRecord(runID)
	if errors.Is(err, sql.ErrNoRows) {
		return GetRunResult{}, &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	if err != nil {
		return GetRunResult{}, internalError(err)
	}
	detail, err := runstate.LoadWithIndex(root, runID, idx)
	if err != nil {
		return GetRunResult{}, internalError(err)
	}
	summary := summaryFromRecord(root, record)
	summary.Status = wireStatus(detail.Summary.Status)
	result := GetRunResult{Summary: summary, Request: detail.Request}
	if includeEvents {
		events := make([]proto.RunEventPayload, 0, len(detail.Events))
		for _, event := range detail.Events {
			events = append(events, payloadFromEvent(event))
		}
		result.Events = events
	}
	return result, nil
}
