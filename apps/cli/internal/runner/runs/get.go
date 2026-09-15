package runs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runindex"
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
		request, ok := reservedRequestRecord(err, record, idx)
		if !ok {
			return GetRunResult{}, internalError(err)
		}
		// startRun durably reserves the Run (index row + exact
		// request.json/run.json evidence) before the engine goroutine that
		// writes those files to disk ever runs: a caller that calls getRun
		// immediately after startRun returns can legitimately race that
		// goroutine. Recover the exact reserved Request instead of either
		// failing outright or fabricating placeholder metadata.
		summary := summaryFromRecord(root, record)
		summary.Status = wireStatus(record.Status)
		result := GetRunResult{Summary: summary, Request: request}
		if includeEvents {
			result.Events = []proto.RunEventPayload{}
		}
		return result, nil
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

// reservedRequestRecord recognizes the narrow window between startRun's
// synchronous, durable reservation and the engine goroutine's asynchronous
// first write of request.json/run.json (see freshlyReservedDetail in
// subscribe.go for the same race on the subscribeRun path). err must be
// exactly a missing-file error, and record must still show the untouched
// initial reservation: "running" with no terminal timestamp yet. When that
// holds, the exact Request evidence committed with the reservation is
// decoded from the idempotency table — never inferred from the lossy index
// projection. Anything else — a genuinely corrupt or unexpectedly absent Run
// directory — still surfaces as an error.
func reservedRequestRecord(err error, record runindex.Record, idx runIndex) (execution.RequestRecord, bool) {
	if !errors.Is(err, os.ErrNotExist) {
		return execution.RequestRecord{}, false
	}
	if record.Status != "running" || record.FinishedAt != "" {
		return execution.RequestRecord{}, false
	}
	reservation, reservationErr := idx.GetReservation(record.ID)
	if reservationErr != nil {
		return execution.RequestRecord{}, false
	}
	var request execution.RequestRecord
	if jsonErr := json.Unmarshal([]byte(reservation.RequestJSON), &request); jsonErr != nil {
		return execution.RequestRecord{}, false
	}
	if request.ID != record.RequestID {
		return execution.RequestRecord{}, false
	}
	return request, true
}
