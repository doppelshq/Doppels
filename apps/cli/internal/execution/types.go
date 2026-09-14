// Package execution runs an already-resolved Capability fulfillment locally.
// It deliberately does not discover manifests or choose a Recipe.
package execution

import (
	"context"
	"errors"
	"io"
	"time"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/runindex"
)

const APIVersion = manifest.APIVersion

var (
	ErrInvalidInvocation = errors.New("invalid invocation")
	ErrRequirements      = errors.New("requirements not satisfied")
	ErrApprovalRejected  = errors.New("approval rejected")
	ErrStepFailed        = errors.New("step failed")
	ErrStepTimedOut      = errors.New("step timed out")
	ErrInterrupted       = errors.New("run interrupted")
	ErrManualRequired    = errors.New("manual fulfillment required")
)

type SchemaReference struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

type DefinitionReference struct {
	Name           string          `json:"name"`
	Version        string          `json:"version"`
	ManifestSHA256 string          `json:"manifestSha256"`
	Schema         SchemaReference `json:"schema"`
}

type ActorReference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type AssignmentReference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type RequestRecord struct {
	APIVersion       string               `json:"apiVersion"`
	Kind             string               `json:"kind"`
	ID               string               `json:"id"`
	CreatedAt        time.Time            `json:"createdAt"`
	IdempotencyKey   string               `json:"idempotencyKey"`
	Origin           string               `json:"origin"`
	Capability       DefinitionReference  `json:"capability"`
	Inputs           map[string]any       `json:"inputs"`
	RequestedBy      ActorReference       `json:"requestedBy"`
	AssignedTo       *AssignmentReference `json:"assignedTo,omitempty"`
	Organization     string               `json:"organization,omitempty"`
	Space            string               `json:"space,omitempty"`
	ShareID          string               `json:"shareId,omitempty"`
	OperatorDecision string               `json:"operatorDecision,omitempty"`
}

type RunRecord struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	ID         string               `json:"id"`
	RequestID  string               `json:"requestId"`
	CreatedAt  time.Time            `json:"createdAt"`
	Capability DefinitionReference  `json:"capability"`
	Recipe     *DefinitionReference `json:"recipe,omitempty"`
	Inputs     map[string]any       `json:"inputs"`
	Executor   ActorReference       `json:"executor"`
	NodeID     string               `json:"nodeId,omitempty"`
}

type RunEvent struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	RunID      string         `json:"runId"`
	Sequence   int            `json:"sequence"`
	OccurredAt time.Time      `json:"occurredAt"`
	Type       string         `json:"type"`
	StepID     string         `json:"stepId,omitempty"`
	Data       map[string]any `json:"data,omitempty"`
}

type ArtifactReference struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
	LocalPath string `json:"-"`
}

func (a ArtifactReference) PublicValue() map[string]any {
	return map[string]any{"artifact": map[string]any{
		"id": a.ID, "filename": a.Filename, "mediaType": a.MediaType,
		"sizeBytes": a.SizeBytes, "sha256": a.SHA256,
	}}
}

type ApprovalRequest struct {
	RunID  string
	StepID string
	Name   string
}

type ApprovalFunc func(context.Context, ApprovalRequest) (bool, error)

type ManualRequest struct {
	RunID         string
	ProjectRoot   string
	Capability    *manifest.Capability
	Recipe        *manifest.Recipe
	Inputs        map[string]any
	ProcedurePath string
}

type FileValue struct {
	Path      string
	MediaType string
}

type ManualResult struct {
	Returns  map[string]any
	Evidence map[string]any
}

type ManualFunc func(context.Context, ManualRequest) (ManualResult, error)

// RunFunc and EventFunc let a coordinator persist the live execution stream.
// They are synchronous: returning nil means the remote side durably accepted
// the record, while an error stops execution rather than silently losing audit
// data.
type RunFunc func(context.Context, RunRecord) error
type EventFunc func(context.Context, RunEvent) error

// BeforeSuccessFunc runs after returns have been materialized but before the
// terminal run_succeeded event. Artifact references still contain their local
// snapshot path here, allowing a coordinator to upload public return bytes
// before publishing the immutable terminal event.
type BeforeSuccessFunc func(context.Context, RunRecord, map[string]any, map[string]any) error

// LogStream identifies the source of a streaming log chunk emitted by a
// running Step. The set is closed: future additions must be additive and
// never repurpose an existing value.
type LogStream string

const (
	LogStreamStdout LogStream = "stdout"
	LogStreamStderr LogStream = "stderr"
)

// LogFunc receives step output chunks as the subprocess writes them. The
// callback fires from the process goroutine and MUST return quickly: it is
// the live wire the Runner will use to forward v1/runLog notifications to
// Desktop subscribers. A nil LogFunc is a no-op.
//
// Chunks are emitted AFTER secret redaction and BEFORE the disk-buffer cap,
// so the live stream and the redacted-on-disk file are byte-identical by
// construction (split-secret state lives in the engine, not the Runner).
type LogFunc func(stream LogStream, chunk []byte)

// Invocation contains definitions that discovery and recipe selection already
// resolved. Recipe is nil for a manual fulfillment without a codified Recipe.
type Invocation struct {
	ProjectRoot      string
	RecipeDirectory  string
	Capability       *manifest.Capability
	CapabilityRef    DefinitionReference
	Recipe           *manifest.Recipe
	RecipeRef        *DefinitionReference
	Inputs           map[string]any
	RequestedBy      ActorReference
	Executor         ActorReference
	AssignedTo       *AssignmentReference
	NodeID           string
	Source           string
	Space            string
	ShareID          string
	IdempotencyKey   string
	RequestID        string
	RequestCreatedAt time.Time
	RunID            string
	// ExistingRequest preserves a Cloud Request at the domain level. Its id,
	// timestamp, actor, inputs and definition reference take precedence over
	// the convenience Request fields above.
	ExistingRequest *RequestRecord
	// PreparedRun is the exact immutable Run record reserved by a durable
	// coordinator before execution begins. When present, initialize writes it
	// verbatim (apart from cloning Inputs) so crash recovery can use the same
	// evidence as the normal path.
	PreparedRun *RunRecord
}

type Options struct {
	ApproveAll    bool
	Approve       ApprovalFunc
	Manual        ManualFunc
	OnRun         RunFunc
	OnEvent       EventFunc
	BeforeSuccess BeforeSuccessFunc
	Stdout        io.Writer
	Stderr        io.Writer
	Environment   []string
	LookupCommand func(string) (string, error)
	Now           func() time.Time
	// RunIndex lets a long-lived coordinator own one SQLite handle for the
	// workspace. Nil preserves standalone CLI behavior by opening an index for
	// each update.
	RunIndex RunIndex
	// LogStreamLimit caps retained stdout/stderr bytes per Step stream.
	// Zero means DefaultLogStreamLimit.
	LogStreamLimit int
	// LogStream, when non-nil, receives every chunk a Step writes to its
	// stdout or stderr AFTER secret redaction but BEFORE the disk-buffer cap
	// that produces the persisted log files. The bytes match the redacted
	// file byte-for-byte (engine keeps split-secret state). The daemon uses
	// this to forward live output to Desktop subscribers; nil keeps the
	// legacy path.
	LogStream LogFunc
	// AfterRequestPersisted is a fault-injection seam used by callers that
	// must test a crash after request.json but before run.json. Nil is a no-op.
	AfterRequestPersisted func() error
}

type RunIndex interface {
	Upsert(runindex.Record) error
	EnqueueOutbox(runID string, payload any) error
}

type StepResult struct {
	ID         string
	ExitCode   int
	Duration   time.Duration
	StdoutPath string
	StderrPath string
	Truncated  bool
	Products   map[string]any
}

type Result struct {
	Status    string
	StateDir  string
	Request   RequestRecord
	Run       RunRecord
	Events    []RunEvent
	Steps     []StepResult
	Returns   map[string]any
	Evidence  map[string]any
	Artifacts map[string]ArtifactReference
}
