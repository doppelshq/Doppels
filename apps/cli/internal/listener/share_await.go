package listener

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/shareclient"
)

// AwaitSharedRequest blocks until the Share receives a Request, closes, or
// ctx ends. alreadyComplete reports a Share whose Run already reached a
// terminal success state (nothing to fulfill).
func AwaitSharedRequest(ctx context.Context, client *shareclient.Client, channel *shareclient.Channel, created *shareclient.ShareCreated, initial *shareclient.PendingState) (*execution.RequestRecord, bool, error) {
	if request, complete, err := RequestFromPending(created, initial); request != nil || complete || err != nil {
		return request, complete, err
	}
	for {
		update, err := channel.Next(ctx)
		if err != nil {
			return nil, false, err
		}
		if update.Recovery != nil {
			if request, complete, err := RequestFromPending(created, update.Recovery); request != nil || complete || err != nil {
				return request, complete, err
			}
			continue
		}
		if update.Message == nil {
			continue
		}
		switch update.Message.Event {
		case "request_available":
			request, err := update.Message.Request()
			if err != nil {
				return nil, false, fmt.Errorf("decode Request: %w", err)
			}
			if err := validateSharedRequest(created.Share, request); err != nil {
				return nil, false, err
			}
			return request, false, nil
		case "share_closed":
			var closed shareClosedPayload
			if err := json.Unmarshal(update.Message.Payload, &closed); err != nil {
				return nil, false, fmt.Errorf("decode Share closure: %w", err)
			}
			if closed.Reason != "completed" {
				return nil, false, fmt.Errorf("Share was %s", closed.Reason)
			}
			pending, err := client.Pending(ctx, created.Share.ID, created.RunnerToken)
			if err != nil {
				return nil, false, err
			}
			return RequestFromPending(created, pending)
		}
	}
}

// RequestFromPending validates the durable pending state of a Share and
// extracts the Request to fulfill, if any.
func RequestFromPending(created *shareclient.ShareCreated, pending *shareclient.PendingState) (*execution.RequestRecord, bool, error) {
	if pending == nil {
		return nil, false, errors.New("Cloud returned no pending Share state")
	}
	if !sharesEqual(created.Share, pending.Share) {
		return nil, false, errors.New("Cloud pending state changed the Share contract")
	}
	if pending.Request == nil {
		if pending.Run != nil || len(pending.Events) != 0 {
			return nil, false, errors.New("Cloud returned a Run without its Request")
		}
		return nil, false, nil
	}
	if err := validateSharedRequest(created.Share, pending.Request); err != nil {
		return nil, false, err
	}
	if pending.Run == nil {
		if len(pending.Events) != 0 {
			return nil, false, errors.New("Cloud returned RunEvents without a Run")
		}
		return pending.Request, false, nil
	}
	if err := validatePendingRun(created.Share, pending.Request, pending.Run, pending.Events); err != nil {
		return nil, false, err
	}
	status, terminal := terminalRunStatus(pending.Events)
	if !terminal {
		return nil, false, fmt.Errorf("Share already contains unfinished Run %s; refusing a duplicate execution", pending.Run.ID)
	}
	if status == "succeeded" {
		return nil, true, nil
	}
	return nil, false, SharedRunError{Status: status, RunID: pending.Run.ID}
}

// SharedRunError reports a Share whose prior Run already terminated in a
// non-success state.
type SharedRunError struct {
	Status string
	RunID  string
}

func (problem SharedRunError) Error() string {
	return fmt.Sprintf("Share Run %s finished with status %s", problem.RunID, problem.Status)
}

type shareClosedPayload struct {
	Reason string `json:"reason"`
}

func validateSharedRequest(share shareclient.Share, request *execution.RequestRecord) error {
	if request == nil || request.APIVersion != execution.APIVersion || request.Kind != "Request" || request.ID == "" || request.IdempotencyKey == "" {
		return errors.New("Cloud returned an incomplete Request")
	}
	if request.ShareID != share.ID || request.Capability != share.CapabilityRevision {
		return errors.New("Cloud returned a Request for another Share or Capability revision")
	}
	return nil
}

func validatePendingRun(share shareclient.Share, request *execution.RequestRecord, run *execution.RunRecord, events []execution.RunEvent) error {
	if run.APIVersion != execution.APIVersion || run.Kind != "Run" || run.ID == "" || run.RequestID != request.ID || run.Capability != share.CapabilityRevision || run.Executor != share.SharedBy || !definitionReferencePointersEqual(run.Recipe, share.Recipe) {
		return errors.New("Cloud returned a Run that does not match the Share")
	}
	for index, event := range events {
		if event.APIVersion != execution.APIVersion || event.Kind != "RunEvent" || event.RunID != run.ID || event.Sequence != index {
			return errors.New("Cloud returned an invalid RunEvent sequence")
		}
	}
	return nil
}

func terminalRunStatus(events []execution.RunEvent) (string, bool) {
	if len(events) == 0 {
		return "running", false
	}
	switch events[len(events)-1].Type {
	case "run_succeeded":
		return "succeeded", true
	case "run_failed":
		return "failed", true
	case "run_cancelled":
		return "cancelled", true
	case "run_interrupted":
		return "interrupted", true
	default:
		return "running", false
	}
}

func sharesEqual(left, right shareclient.Share) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func definitionReferencePointersEqual(left, right *execution.DefinitionReference) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
