package runnerclient

import (
	"context"

	"doppels.so/cli/internal/runner/proto"
)

// Request/result shapes below mirror the wire contract exactly as
// implemented server-side (internal/runner/runs/{rpc,get,list,logs,
// approvals,subscribe}.go and internal/runner/workspace/rpc.go) rather than
// importing those packages directly: runnerclient is a client-side,
// dependency-light package Desktop will reuse via FFI, and the wire format
// (not the server's internal Go types) is the actual contract.

// Ping calls v1/ping and returns the server's pong timestamp string.
func (c *Client) Ping(ctx context.Context) (string, error) {
	var result struct {
		Pong string `json:"pong"`
	}
	if err := c.Call(ctx, "v1/ping", map[string]any{}, &result); err != nil {
		return "", err
	}
	return result.Pong, nil
}

// GetNodeStatus calls v1/getNodeStatus.
func (c *Client) GetNodeStatus(ctx context.Context) (proto.NodeStatus, error) {
	var result proto.NodeStatus
	err := c.Call(ctx, "v1/getNodeStatus", map[string]any{}, &result)
	return result, err
}

// ListWorkspaces calls v1/listWorkspaces.
func (c *Client) ListWorkspaces(ctx context.Context) ([]proto.WorkspaceSummary, error) {
	var result []proto.WorkspaceSummary
	err := c.Call(ctx, "v1/listWorkspaces", map[string]any{}, &result)
	return result, err
}

// AddWorkspace calls v1/addWorkspace.
func (c *Client) AddWorkspace(ctx context.Context, root string) (proto.WorkspaceSummary, error) {
	var result proto.WorkspaceSummary
	err := c.Call(ctx, "v1/addWorkspace", map[string]any{"root": root}, &result)
	return result, err
}

// RemoveWorkspace calls v1/removeWorkspace.
func (c *Client) RemoveWorkspace(ctx context.Context, root string) error {
	var result map[string]any
	return c.Call(ctx, "v1/removeWorkspace", map[string]any{"root": root}, &result)
}

// ListCapabilities calls v1/listCapabilities. An empty workspace aggregates
// every registered workspace.
func (c *Client) ListCapabilities(ctx context.Context, workspace string) ([]proto.CapabilitySummary, error) {
	var result []proto.CapabilitySummary
	err := c.Call(ctx, "v1/listCapabilities", map[string]any{"workspace": workspace}, &result)
	return result, err
}

// GetCapabilityResult is the v1/getCapability response.
type GetCapabilityResult struct {
	Summary  proto.CapabilitySummary `json:"summary"`
	Manifest map[string]any          `json:"manifest"`
	Inputs   map[string]any          `json:"inputs"`
	Outputs  map[string]any          `json:"outputs"`
	Runs     int                     `json:"runs"`
}

// GetCapability calls v1/getCapability.
func (c *Client) GetCapability(ctx context.Context, workspace, name, version string) (GetCapabilityResult, error) {
	var result GetCapabilityResult
	err := c.Call(ctx, "v1/getCapability", map[string]any{
		"workspace": workspace, "name": name, "version": version,
	}, &result)
	return result, err
}

// StartRunParams is the v1/startRun request (RFC §9).
type StartRunParams struct {
	Workspace      string         `json:"workspace"`
	Capability     string         `json:"capability"`
	Recipe         string         `json:"recipe,omitempty"`
	Inputs         map[string]any `json:"inputs"`
	ApprovalMode   string         `json:"approvalMode"`
	IdempotencyKey string         `json:"idempotencyKey"`
}

// StartRunResult is the v1/startRun response.
type StartRunResult struct {
	RequestID string `json:"requestId"`
	RunID     string `json:"runId"`
}

// StartRun calls v1/startRun.
func (c *Client) StartRun(ctx context.Context, params StartRunParams) (StartRunResult, error) {
	var result StartRunResult
	err := c.Call(ctx, "v1/startRun", params, &result)
	return result, err
}

// CancelRun calls v1/cancelRun and returns the Run's resulting status.
func (c *Client) CancelRun(ctx context.Context, runID, reason string) (string, error) {
	var result struct {
		Status string `json:"status"`
	}
	err := c.Call(ctx, "v1/cancelRun", map[string]any{"runId": runID, "reason": reason}, &result)
	return result.Status, err
}

// GetRunResult is the v1/getRun response.
type GetRunResult struct {
	Summary proto.RunSummary        `json:"summary"`
	Request map[string]any          `json:"request"`
	Events  []proto.RunEventPayload `json:"events,omitempty"`
}

// GetRun calls v1/getRun.
func (c *Client) GetRun(ctx context.Context, runID string, includeEvents bool) (GetRunResult, error) {
	var result GetRunResult
	err := c.Call(ctx, "v1/getRun", map[string]any{
		"runId": runID, "includeEvents": includeEvents,
	}, &result)
	return result, err
}

// ListRunsParams is the v1/listRuns request.
type ListRunsParams struct {
	Workspace  string `json:"workspace,omitempty"`
	Capability string `json:"capability,omitempty"`
	Status     string `json:"status,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
}

// ListRunsResult is the v1/listRuns response.
type ListRunsResult struct {
	Runs       []proto.RunSummary `json:"runs"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// ListRuns calls v1/listRuns.
func (c *Client) ListRuns(ctx context.Context, params ListRunsParams) (ListRunsResult, error) {
	var result ListRunsResult
	err := c.Call(ctx, "v1/listRuns", params, &result)
	return result, err
}

// GetRunLogsParams is the v1/getRunLogs request.
type GetRunLogsParams struct {
	RunID  string `json:"runId"`
	StepID string `json:"stepId,omitempty"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// LogFile describes one confined per-(step,stream) log file (RFC §9).
type LogFile struct {
	StepID    string `json:"stepId"`
	Stream    string `json:"stream"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

// GetRunLogsResult is the v1/getRunLogs response.
type GetRunLogsResult struct {
	Files   []LogFile `json:"files"`
	Content *string   `json:"content,omitempty"`
}

// GetRunLogs calls v1/getRunLogs.
func (c *Client) GetRunLogs(ctx context.Context, params GetRunLogsParams) (GetRunLogsResult, error) {
	var result GetRunLogsResult
	err := c.Call(ctx, "v1/getRunLogs", params, &result)
	return result, err
}

// PendingApproval is one entry of the v1/listPendingApprovals response.
type PendingApproval struct {
	RunID       string `json:"runId"`
	StepID      string `json:"stepId"`
	Name        string `json:"name"`
	RequestedAt string `json:"requestedAt"`
}

// ListPendingApprovals calls v1/listPendingApprovals.
func (c *Client) ListPendingApprovals(ctx context.Context) ([]PendingApproval, error) {
	var result []PendingApproval
	err := c.Call(ctx, "v1/listPendingApprovals", map[string]any{}, &result)
	return result, err
}

// DecideApproval calls v1/decideApproval. decision must be "approve" or
// "reject".
func (c *Client) DecideApproval(ctx context.Context, runID, stepID, decision string) error {
	var result map[string]any
	return c.Call(ctx, "v1/decideApproval", map[string]any{
		"runId": runID, "stepId": stepID, "decision": decision,
	}, &result)
}

// SubscribeRunResult is the v1/subscribeRun response: the replay batch plus
// the Run's current status. Live events follow as notifications on
// OnNotification (method "v1/runEvent").
type SubscribeRunResult struct {
	Events []proto.RunEventPayload `json:"events"`
	Status string                  `json:"status"`
}

// SubscribeRun calls v1/subscribeRun.
func (c *Client) SubscribeRun(ctx context.Context, runID string, fromSequence int) (SubscribeRunResult, error) {
	var result SubscribeRunResult
	err := c.Call(ctx, "v1/subscribeRun", map[string]any{
		"runId": runID, "fromSequence": fromSequence,
	}, &result)
	return result, err
}

// SubscribeRunLogs calls v1/subscribeRunLogs (capability liveLogs). Live
// chunks follow as notifications on OnNotification (method "v1/runLog").
func (c *Client) SubscribeRunLogs(ctx context.Context, runID, stepID string) (bool, error) {
	var result struct {
		Active bool `json:"active"`
	}
	err := c.Call(ctx, "v1/subscribeRunLogs", map[string]any{
		"runId": runID, "stepId": stepID,
	}, &result)
	return result.Active, err
}

// Shutdown calls v1/shutdown. The Runner closes the connection once the ack
// is flushed.
func (c *Client) Shutdown(ctx context.Context, reason string) error {
	var result map[string]any
	return c.Call(ctx, "v1/shutdown", map[string]any{"reason": reason}, &result)
}
