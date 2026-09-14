package proto

// Shared payload types (RFC §8). Field names are the wire contract in
// camelCase; JSON Schemas will be frozen from these structs when v1 freezes.

type NodeStatus struct {
	State           string             `json:"state"` // "online" | "degraded"
	RunnerVersion   string             `json:"runnerVersion"`
	ProtocolVersion int                `json:"protocolVersion"`
	StartedAt       string             `json:"startedAt"` // RFC3339
	Workspaces      []WorkspaceSummary `json:"workspaces"`
	Cloud           *CloudStatus       `json:"cloud"`
}

type CloudStatus struct {
	Connected    bool    `json:"connected"`
	Server       string  `json:"server"`
	Organization *string `json:"organization"`
}

type WorkspaceSummary struct {
	Root         string  `json:"root"`
	Space        string  `json:"space"`
	Capabilities int     `json:"capabilities"`
	Recipes      int     `json:"recipes"`
	GitBranch    *string `json:"gitBranch"`
	Health       string  `json:"health"` // "ok" | "invalidManifests" | "missingRoot"
}

type CapabilitySummary struct {
	Name           string             `json:"name"`
	Version        string             `json:"version"`
	Workspace      string             `json:"workspace"`
	Space          string             `json:"space"`
	ManifestSHA256 string             `json:"manifestSha256"`
	Runtime        string             `json:"runtime"` // "shell" | "manual" | "none"
	Recipe         *RecipeReference   `json:"recipe"`
	Pin            string             `json:"pin"` // "pinned" | "stale" | "unpinned"
	Readiness      []RequirementCheck `json:"readiness"`
}

type RecipeReference struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	ManifestSHA256 string `json:"manifestSha256"`
}

type RequirementCheck struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
}

// RunEventPayload mirrors the engine RunEvent vocabulary (events.jsonl and
// cloud RunEvent): sequence is contiguous from 0, terminal event is unique.
type RunEventPayload struct {
	RunID      string         `json:"runId"`
	Sequence   int            `json:"sequence"`
	Type       string         `json:"type"`
	StepID     string         `json:"stepId,omitempty"`
	Data       map[string]any `json:"data,omitempty"`
	OccurredAt string         `json:"occurredAt"` // RFC3339
}

type RunSummary struct {
	RunID      string  `json:"runId"`
	RequestID  string  `json:"requestId"`
	Capability string  `json:"capability"`
	Recipe     *string `json:"recipe"`
	NodeID     string  `json:"nodeId"`
	Status     string  `json:"status"`
	Source     string  `json:"source"`
	Workspace  string  `json:"workspace"`
	CreatedAt  string  `json:"createdAt"`
	FinishedAt *string `json:"finishedAt"`
}

// InitializeParams is the v1/initialize request payload (RFC §6).
type InitializeParams struct {
	Token  string `json:"token"`
	Client struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"client"`
}

// InitializeResult is the v1/initialize response payload.
type InitializeResult struct {
	ProtocolVersion int        `json:"protocolVersion"`
	RunnerVersion   string     `json:"runnerVersion"`
	Capabilities    []string   `json:"capabilities"`
	NodeStatus      NodeStatus `json:"nodeStatus"`
}

// NodeEventKind enumerates v1/nodeEvent kinds (RFC §10).
const (
	NodeEventWorkspaceAdded    = "workspaceAdded"
	NodeEventWorkspaceRemoved  = "workspaceRemoved"
	NodeEventWorkspaceChanged  = "workspaceChanged"
	NodeEventRunStarted        = "runStarted"
	NodeEventRunFinished       = "runFinished"
	NodeEventApprovalPending   = "approvalPending"
	NodeEventCloudConnected    = "cloudConnected"
	NodeEventCloudDisconnected = "cloudDisconnected"
	NodeEventNodeDegraded      = "nodeDegraded"
	NodeEventRunEventGap       = "runEventGap"
)

// NodeEvent is the v1/nodeEvent notification payload.
type NodeEvent struct {
	Kind    string `json:"kind"`
	Payload any    `json:"payload"`
}

// RunLogChunk is the v1/runLog notification payload (capability liveLogs).
type RunLogChunk struct {
	RunID     string `json:"runId"`
	StepID    string `json:"stepId"`
	Stream    string `json:"stream"` // "stdout" | "stderr"
	Data      string `json:"data"`
	Truncated bool   `json:"truncated"`
}
