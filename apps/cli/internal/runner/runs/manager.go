// Package runs coordinates Runner-owned local executions and their durable
// per-workspace indexes.
package runs

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/localstate"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/runindex"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/workspace"
	"doppels.so/cli/internal/runstate"
)

// runIndex is the subset of *runindex.Index the Manager depends on. It
// exists so tests can inject a decorator (e.g. a fault-injecting Upsert) to
// deterministically reproduce persistence-ordering bugs without sleeps or
// timing races.
type runIndex interface {
	Get(id string) (runindex.Record, error)
	Upsert(record runindex.Record) error
	ReserveStart(record runindex.Record, key, fingerprint string, evidence runindex.ReservationEvidence) (runindex.IdempotencyRecord, bool, error)
	GetReservation(runID string) (runindex.IdempotencyRecord, error)
	EnqueueOutbox(runID string, payload any) error
	CommitTerminal(record runindex.Record, payload any) (bool, error)
	List() ([]runindex.Record, error)
	ListPage(query runindex.ListQuery) (runindex.Page, error)
	Close() error
}

type Config struct {
	NodeID      string
	Environment []string
	Now         func() time.Time
	Log         func(format string, args ...any)
	OnStarted   func(proto.RunSummary)
	OnFinished  func(proto.RunSummary)
	// EmitNodeEvent publishes Runner-wide lifecycle events. It is optional so
	// embedders that do not expose node subscriptions keep working.
	EmitNodeEvent func(proto.NodeEvent)
}

type StartResult struct {
	RequestID string `json:"requestId"`
	RunID     string `json:"runId"`
}

type activeRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	root   string
}

type Manager struct {
	ctx        context.Context
	cancel     context.CancelFunc
	workspaces *workspace.Service
	config     Config

	mu      sync.Mutex
	indexes map[string]runIndex
	active  map[string]*activeRun
	closed  bool
	wg      sync.WaitGroup

	terminalMu sync.Mutex

	subsMu sync.Mutex
	subs   map[string][]*runSubscriber

	pendingApprovalsMu  sync.Mutex
	pendingApprovals    map[string]chan approvalDecision
	pendingApprovalInfo map[string]PendingApproval

	// testBeforeReserve is a test-only seam invoked synchronously right
	// before Start would durably reserve, letting tests deterministically
	// interleave Start with a concurrent Close. Always nil in production.
	testBeforeReserve func()
	// testStopAfterReserve simulates process loss immediately after the
	// reservation transaction commits, before an engine goroutine exists.
	testStopAfterReserve bool
	// testAfterRequestPersisted simulates process loss after request.json and
	// before run.json.
	testAfterRequestPersisted func() error
	// testBeforeForward/testAfterForward are test-only scheduling seams for
	// pinning a live subscriber forwarder's drain window.
	testBeforeForward func()
	testAfterForward  func()

	// openIndex constructs the runIndex for a workspace root. Overridable in
	// tests to wrap the real *runindex.Index in a fault-injecting decorator
	// (e.g. a failing Upsert), reproducing persistence-ordering bugs
	// deterministically instead of via timing.
	openIndex func(root string) (runIndex, error)
}

func NewManager(ctx context.Context, workspaces *workspace.Service, config Config) *Manager {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	if config.NodeID == "" {
		config.NodeID = "local"
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.Log == nil {
		config.Log = func(string, ...any) {}
	}
	return &Manager{
		ctx: ctx, cancel: cancel, workspaces: workspaces, config: config,
		indexes: make(map[string]runIndex), active: make(map[string]*activeRun),
		subs:                make(map[string][]*runSubscriber),
		pendingApprovals:    make(map[string]chan approvalDecision),
		pendingApprovalInfo: make(map[string]PendingApproval),
		openIndex:           func(root string) (runIndex, error) { return runindex.Open(root) },
	}
}

type startParams struct {
	Workspace      string          `json:"workspace"`
	Capability     string          `json:"capability"`
	Recipe         string          `json:"recipe"`
	Inputs         json.RawMessage `json:"inputs"`
	ApprovalMode   string          `json:"approvalMode"`
	IdempotencyKey string          `json:"idempotencyKey"`
}

func (m *Manager) Start(clientName string, params []byte) (StartResult, *proto.Error) {
	canonical, err := proto.CanonicalJSON(params)
	if err != nil {
		return StartResult{}, invalidParams("invalid canonical JSON payload")
	}
	var request startParams
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		return StartResult{}, invalidParams("params must be an object")
	}
	if request.Workspace == "" || request.Capability == "" {
		return StartResult{}, invalidParams("workspace and capability are required")
	}
	if request.ApprovalMode != "auto" && request.ApprovalMode != "interactive" {
		return StartResult{}, invalidParams("approvalMode must be auto or interactive")
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 200 || strings.ContainsRune(request.IdempotencyKey, 0) {
		return StartResult{}, invalidParams("idempotencyKey must contain 1 to 200 safe characters")
	}
	if len(request.Inputs) == 0 {
		request.Inputs = json.RawMessage(`{}`)
	}
	var supplied map[string]any
	inputDecoder := json.NewDecoder(bytes.NewReader(request.Inputs))
	inputDecoder.UseNumber()
	if err := inputDecoder.Decode(&supplied); err != nil || supplied == nil {
		return StartResult{}, invalidParams("inputs must be an object")
	}

	resolved, err := m.workspaces.ResolveExecution(request.Workspace, request.Capability, request.Recipe)
	if err != nil {
		return StartResult{}, resolutionError(err)
	}
	if resolved.StalePin {
		return StartResult{}, &proto.Error{Code: proto.CodeStalePin, Message: "pinned Capability or Recipe is stale"}
	}
	inputs, err := execution.ValidateInputs(resolved.Capability.Value, supplied)
	if err != nil {
		return StartResult{}, &proto.Error{Code: proto.CodeInvalidInputs, Message: "inputs do not satisfy the Capability contract", Data: map[string]any{"diagnostics": []string{err.Error()}}}
	}

	capability := resolved.Capability.Value.Metadata.Name + "@" + resolved.Capability.Value.Metadata.Version
	var recipe *string
	if resolved.Recipe != nil {
		value := resolved.Recipe.Value.Metadata.Name + "@" + resolved.Recipe.Value.Metadata.Version
		recipe = &value
	}
	fingerprint, err := requestFingerprint(capability, recipe, request.Inputs)
	if err != nil {
		return StartResult{}, invalidParams("inputs cannot be canonicalized")
	}
	requestID, err := newID()
	if err != nil {
		return StartResult{}, internalError(err)
	}
	runID, err := newID()
	if err != nil {
		return StartResult{}, internalError(err)
	}
	createdAt := m.config.Now().UTC().Truncate(time.Millisecond)
	idx, err := m.index(resolved.Root)
	if err != nil {
		return StartResult{}, internalError(err)
	}
	recipeText := ""
	if recipe != nil {
		recipeText = *recipe
	}
	record := runindex.Record{
		ID: runID, RequestID: requestID, Status: "running", Source: sourceForClient(clientName),
		Capability: capability, Recipe: recipeText, NodeID: m.config.NodeID,
		CreatedAt: createdAt.Format(time.RFC3339Nano),
		StateDir:  filepath.Join(resolved.Root, ".doppels", "runs", runID),
	}
	capabilityRef := execution.ReferenceCapability(resolved.Capability)
	var recipeRef *execution.DefinitionReference
	if resolved.Recipe != nil {
		value := execution.ReferenceRecipe(*resolved.Recipe)
		recipeRef = &value
	}
	requestRecord := execution.RequestRecord{
		APIVersion: execution.APIVersion, Kind: "Request", ID: requestID,
		CreatedAt: createdAt, IdempotencyKey: request.IdempotencyKey, Origin: "cli",
		Capability: capabilityRef, Inputs: inputs,
		RequestedBy: execution.ActorReference{Kind: "identity", ID: "local-operator"},
		Space:       resolved.Space,
	}
	runRecord := execution.RunRecord{
		APIVersion: execution.APIVersion, Kind: "Run", ID: runID, RequestID: requestID,
		CreatedAt: createdAt, Capability: capabilityRef, Recipe: recipeRef, Inputs: inputs,
		Executor: execution.ActorReference{Kind: "service", ID: "doppels-runner"},
	}
	if resolved.Recipe != nil && resolved.Recipe.Value.Runtime == "shell" {
		runRecord.NodeID = m.config.NodeID
	}
	requestJSON, err := json.Marshal(requestRecord)
	if err != nil {
		return StartResult{}, internalError(err)
	}
	runJSON, err := json.Marshal(runRecord)
	if err != nil {
		return StartResult{}, internalError(err)
	}
	evidence := runindex.ReservationEvidence{RequestJSON: string(requestJSON), RunJSON: string(runJSON)}
	if m.testBeforeReserve != nil {
		m.testBeforeReserve()
	}
	// The closed check and the durable reservation must be atomic with
	// respect to Close: a reservation that commits after Close has already
	// flipped m.closed would return Busy to its caller while leaving a
	// "running" row with no goroutine ever spawned to resume or finalize it
	// (findings review, PR6). Holding m.mu across ReserveStart serializes it
	// with Close, which takes the same lock to set closed and cancel active
	// Runs.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return StartResult{}, &proto.Error{Code: proto.CodeBusy, Message: "runner is shutting down"}
	}
	reservation, created, err := idx.ReserveStart(record, request.IdempotencyKey, fingerprint, evidence)
	if err != nil {
		m.mu.Unlock()
		if errors.Is(err, runindex.ErrIdempotencyConflict) {
			return StartResult{}, invalidParams("idempotencyKey was already used with different capability, recipe, or inputs")
		}
		return StartResult{}, internalError(err)
	}
	result := StartResult{RequestID: reservation.RequestID, RunID: reservation.RunID}
	if !created {
		m.mu.Unlock()
		return result, nil
	}
	if m.testStopAfterReserve {
		m.mu.Unlock()
		return result, nil
	}

	runCtx, cancel := context.WithCancel(m.ctx)
	active := &activeRun{cancel: cancel, done: make(chan struct{}), root: resolved.Root}
	m.active[runID] = active
	m.wg.Add(1)
	m.mu.Unlock()

	if m.config.OnStarted != nil {
		m.config.OnStarted(summaryFromRecord(resolved.Root, record))
	}
	go m.execute(runCtx, active, idx, resolved, inputs, request.IdempotencyKey, result, record.Source, request.ApprovalMode, createdAt, requestRecord, runRecord)
	return result, nil
}

func (m *Manager) execute(ctx context.Context, active *activeRun, idx runIndex, resolved workspace.Execution, inputs map[string]any, key string, ids StartResult, source, approvalMode string, createdAt time.Time, requestRecord execution.RequestRecord, runRecord execution.RunRecord) {
	defer m.wg.Done()
	defer close(active.done)
	defer active.cancel()
	defer func() {
		m.mu.Lock()
		delete(m.active, ids.RunID)
		m.mu.Unlock()
	}()

	invocation := execution.Invocation{
		ProjectRoot: resolved.Root, RecipeDirectory: resolved.RecipeDirectory,
		Capability: resolved.Capability.Value, CapabilityRef: execution.ReferenceCapability(resolved.Capability),
		Inputs: inputs, RequestedBy: execution.ActorReference{Kind: "identity", ID: "local-operator"},
		Executor: execution.ActorReference{Kind: "service", ID: "doppels-runner"},
		NodeID:   m.config.NodeID, Source: source, Space: resolved.Space, IdempotencyKey: key,
		RequestID: ids.RequestID, RequestCreatedAt: createdAt, RunID: ids.RunID,
		ExistingRequest: &requestRecord, PreparedRun: &runRecord,
	}
	if resolved.Recipe != nil {
		reference := execution.ReferenceRecipe(*resolved.Recipe)
		invocation.Recipe = resolved.Recipe.Value
		invocation.RecipeRef = &reference
	}
	options := execution.Options{
		ApproveAll: approvalMode == "auto", Environment: m.config.Environment,
		Now:                   func() time.Time { return m.config.Now().UTC().Truncate(time.Millisecond) },
		RunIndex:              idx,
		AfterRequestPersisted: m.testAfterRequestPersisted,
		OnEvent: func(_ context.Context, event execution.RunEvent) error {
			m.broadcast(ids.RunID, payloadFromEvent(event))
			return nil
		},
	}
	if approvalMode == "interactive" {
		options.Approve = func(ctx context.Context, request execution.ApprovalRequest) (bool, error) {
			return m.awaitApproval(ctx, request)
		}
	}
	result, err := execution.Execute(ctx, invocation, options)
	if err != nil && !errors.Is(err, execution.ErrManualRequired) && !errors.Is(err, execution.ErrInterrupted) {
		m.config.Log("Run %s failed: %v", ids.RunID, err)
	}
	if result.Status != "running" && result.Status != "pending_manual" && m.config.OnFinished != nil {
		// Result.Status reflects the engine's in-memory view, set before its
		// own terminal CommitTerminal write was even attempted (see
		// execution/runner.go): that write can fail independently, leaving
		// the index non-terminal with no outbox item while Result.Status
		// already reads "succeeded"/"failed"/etc. Trust only a durable
		// record that actually matches — status and a set FinishedAt — never
		// Result.Status alone. If persistence didn't actually land, this
		// Run stays reconcilable (e.g. via Cancel's idempotent repair path,
		// which itself fires OnFinished once persistence catches up).
		if record, getErr := idx.Get(ids.RunID); getErr == nil &&
			record.Status == result.Status && record.FinishedAt != "" {
			m.config.OnFinished(summaryFromRecord(resolved.Root, record))
		}
	}
}

func (m *Manager) index(root string) (runIndex, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("run manager is closed")
	}
	if idx := m.indexes[root]; idx != nil {
		return idx, nil
	}
	idx, err := m.openIndex(root)
	if err != nil {
		return nil, err
	}
	if err := reconcileOrphanedReservations(root, idx, m.config.Now); err != nil {
		idx.Close()
		return nil, err
	}
	m.indexes[root] = idx
	return idx, nil
}

// reconcileOrphanedReservations resolves Runs whose durable reservation
// committed (a runindex row exists) but whose engine goroutine never reached
// runner.initialize() — the Runner process exited between ReserveStart and
// Execute. This is deliberately narrower than full crash recovery: a Run
// whose on-disk request.json/run.json DO exist but never reached a terminal
// event is a mid-Step kill -9 case, out of PR6's scope per the
// desktop-first-runner plan (PR10 owns supervision/PGID recovery); it is
// left untouched here and remains resolvable via an explicit cancelRun,
// which already finalizes a durable Run from its on-disk state.
//
// Without this reconciliation, a durable idempotent retry for an orphaned
// reservation would return a stable {requestId, runId} that can never be
// loaded (no files were ever written) and would stay "running" forever.
func reconcileOrphanedReservations(root string, idx runIndex, now func() time.Time) error {
	records, err := idx.List()
	if err != nil {
		return err
	}
	for _, record := range records {
		if isTerminal(record.Status) || record.Status == "pending_manual" {
			continue
		}
		runDir := filepath.Join(root, ".doppels", "runs", record.ID)
		if _, statErr := os.Stat(filepath.Join(runDir, "run.json")); statErr == nil {
			continue // engine initialized; resuming/finalizing it is PR10 territory.
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		reservation, err := idx.GetReservation(record.ID)
		if err != nil {
			return fmt.Errorf("load orphaned Run %s evidence: %w", record.ID, err)
		}
		var request execution.RequestRecord
		var run execution.RunRecord
		if err := json.Unmarshal([]byte(reservation.RequestJSON), &request); err != nil {
			return fmt.Errorf("decode orphaned Run %s Request evidence: %w", record.ID, err)
		}
		if err := json.Unmarshal([]byte(reservation.RunJSON), &run); err != nil {
			return fmt.Errorf("decode orphaned Run %s Run evidence: %w", record.ID, err)
		}
		if request.ID != record.RequestID || run.ID != record.ID || run.RequestID != request.ID {
			return fmt.Errorf("orphaned Run %s reservation evidence has inconsistent identifiers", record.ID)
		}
		if err := materializeOrphanedReservation(root, runDir, record, request, run, now, idx); err != nil {
			return fmt.Errorf("reconcile orphaned Run %s: %w", record.ID, err)
		}
	}
	return nil
}

// materializeOrphanedReservation completes the exact immutable Request/Run
// evidence committed with the reservation, then records that execution never
// began. It never infers inputs, keys, Space, source, or definition hashes
// from the lossy index projection.
func materializeOrphanedReservation(root, runDir string, record runindex.Record, request execution.RequestRecord, run execution.RunRecord, now func() time.Time, idx runIndex) error {
	// A partially-initialized directory (crash mid-Open, before run.json) has
	// no observers depending on its contents: safe to clear and retry.
	if err := os.RemoveAll(runDir); err != nil {
		return err
	}
	store, err := localstate.Open(root, record.ID)
	if err != nil {
		return err
	}
	occurred := now().UTC().Truncate(time.Millisecond)
	if err := store.WriteRequest(request); err != nil {
		return err
	}
	if err := store.WriteRun(run); err != nil {
		return err
	}
	created := execution.RunEvent{APIVersion: execution.APIVersion, Kind: "RunEvent", RunID: record.ID, Sequence: 0, OccurredAt: run.CreatedAt, Type: "run_created"}
	if err := store.AppendEvent(created); err != nil {
		return err
	}
	interrupted := execution.RunEvent{
		APIVersion: execution.APIVersion, Kind: "RunEvent", RunID: record.ID, Sequence: 1, OccurredAt: occurred, Type: "run_interrupted",
		Data: map[string]any{"reason": "runner restarted before this Run began executing"},
	}
	if err := store.AppendEvent(interrupted); err != nil {
		return err
	}
	record.Status = "interrupted"
	record.FinishedAt = occurred.Format(time.RFC3339Nano)
	_, err = idx.CommitTerminal(record, map[string]any{
		"id": record.ID, "requestId": record.RequestID, "status": record.Status,
		"capability": record.Capability, "recipe": record.Recipe,
		"createdAt": record.CreatedAt, "finishedAt": record.FinishedAt,
	})
	return err
}

func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// Cancel is idempotent. Active executions are cancelled through the engine's
// context (which terminates the process group); durable manual or orphaned
// non-terminal Runs receive exactly one terminal event under terminalMu.
func (m *Manager) Cancel(runID, _ string) (string, *proto.Error) {
	if !safeRunID(runID) {
		return "", &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	m.terminalMu.Lock()
	defer m.terminalMu.Unlock()

	m.mu.Lock()
	active := m.active[runID]
	m.mu.Unlock()
	if active != nil {
		active.cancel()
		<-active.done
	}
	root, idx, record, err := m.findRecord(runID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	if err != nil {
		return "", internalError(err)
	}
	detail, err := runstate.LoadWithIndex(root, runID, idx)
	if err != nil {
		return "", internalError(err)
	}
	if isTerminal(detail.Summary.Status) {
		// events.jsonl already has the terminal event (e.g. a previous
		// Cancel appended it but failed to persist the index update, or the
		// engine finished independently); reconcile the stale index row
		// without re-emitting the event, but still broadcast it — a live
		// subscriber that was registered before this happened must not be
		// left waiting forever for a terminal event that already occurred.
		if len(detail.Events) == 0 {
			return "", internalError(errors.New("terminal Run has no durable terminal event"))
		}
		terminal := detail.Events[len(detail.Events)-1]
		if !isTerminalEventType(terminal.Type) {
			return "", internalError(errors.New("terminal Run's last event is not terminal"))
		}
		record.Status = detail.Summary.Status
		record.FinishedAt = terminal.OccurredAt.UTC().Format(time.RFC3339Nano)
		repaired, err := idx.CommitTerminal(record, terminalOutboxPayload(record))
		if err != nil {
			return "", internalError(err)
		}
		if repaired {
			m.broadcast(runID, payloadFromEvent(terminal))
			if m.config.OnFinished != nil {
				m.config.OnFinished(summaryFromRecord(root, record))
			}
		}
		return wireStatus(record.Status), nil
	}
	store, err := localstate.Resume(root, runID)
	if err != nil {
		return "", internalError(err)
	}
	eventType, status := "run_interrupted", "interrupted"
	if record.Status == "pending_manual" {
		eventType, status = "run_cancelled", "cancelled"
	}
	event := execution.RunEvent{
		APIVersion: execution.APIVersion, Kind: "RunEvent", RunID: runID,
		Sequence: len(detail.Events), OccurredAt: m.config.Now().UTC().Truncate(time.Millisecond), Type: eventType,
	}
	if err := store.AppendEvent(event); err != nil {
		return "", internalError(err)
	}
	record.Status = status
	record.FinishedAt = event.OccurredAt.Format(time.RFC3339Nano)
	// The index write must complete before any notification: a subscriber
	// reacting to the terminal event (e.g. by calling listRuns) must never
	// observe a stale, non-terminal index row.
	committed, err := idx.CommitTerminal(record, terminalOutboxPayload(record))
	if err != nil {
		return "", internalError(err)
	}
	if committed {
		m.broadcast(runID, payloadFromEvent(event))
		if m.config.OnFinished != nil {
			m.config.OnFinished(summaryFromRecord(root, record))
		}
	}
	return wireStatus(status), nil
}

func terminalOutboxPayload(record runindex.Record) map[string]any {
	return map[string]any{
		"id": record.ID, "requestId": record.RequestID, "status": record.Status,
		"capability": record.Capability, "recipe": record.Recipe,
		"createdAt": record.CreatedAt, "finishedAt": record.FinishedAt,
	}
}

func (m *Manager) findRecord(runID string) (string, runIndex, runindex.Record, error) {
	for _, summary := range m.workspaces.ListWorkspaces() {
		if summary.Health == "missingRoot" {
			continue
		}
		idx, err := m.index(summary.Root)
		if err != nil {
			return "", nil, runindex.Record{}, err
		}
		record, err := idx.Get(runID)
		if err == nil {
			return summary.Root, idx, record, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", nil, runindex.Record{}, err
		}
	}
	return "", nil, runindex.Record{}, sql.ErrNoRows
}

func isTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "interrupted":
		return true
	default:
		return false
	}
}

func wireStatus(status string) string {
	if status == "pending_manual" {
		return "pendingManual"
	}
	return status
}

func safeRunID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	for _, active := range m.active {
		active.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	var result error
	for root, idx := range m.indexes {
		if err := idx.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close run index %s: %w", root, err))
		}
	}
	m.indexes = make(map[string]runIndex)
	return result
}

func requestFingerprint(capability string, recipe *string, inputs json.RawMessage) (string, error) {
	capabilityJSON, _ := json.Marshal(capability)
	recipeJSON := []byte("null")
	if recipe != nil {
		recipeJSON, _ = json.Marshal(*recipe)
	}
	canonicalInputs, err := proto.CanonicalJSON(inputs)
	if err != nil {
		return "", err
	}
	payload := make([]byte, 0, len(capabilityJSON)+len(recipeJSON)+len(canonicalInputs)+40)
	payload = append(payload, `{"capability":`...)
	payload = append(payload, capabilityJSON...)
	payload = append(payload, `,"recipe":`...)
	payload = append(payload, recipeJSON...)
	payload = append(payload, `,"inputs":`...)
	payload = append(payload, canonicalInputs...)
	payload = append(payload, '}')
	return proto.Fingerprint(payload)
}

func sourceForClient(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cli", "doppels-cli":
		return "cli"
	case "desktop", "doppels-desktop":
		return "desktop"
	default:
		return "local"
	}
}

func newID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hex := fmt.Sprintf("%x", value)
	return strings.Join([]string{hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32]}, "-"), nil
}

func summaryFromRecord(root string, record runindex.Record) proto.RunSummary {
	var recipe *string
	if record.Recipe != "" {
		value := record.Recipe
		recipe = &value
	}
	var finished *string
	if record.FinishedAt != "" {
		value := record.FinishedAt
		finished = &value
	}
	status := record.Status
	if status == "pending_manual" {
		status = "pendingManual"
	}
	return proto.RunSummary{
		RunID: record.ID, RequestID: record.RequestID, Capability: record.Capability,
		Recipe: recipe, NodeID: record.NodeID, Status: status, Source: record.Source,
		Workspace: root, CreatedAt: record.CreatedAt, FinishedAt: finished,
	}
}

func invalidParams(message string) *proto.Error {
	return &proto.Error{Code: proto.CodeInvalidParams, Message: message}
}

func internalError(err error) *proto.Error {
	return &proto.Error{Code: proto.CodeInternal, Message: "runner persistence failed", Data: map[string]any{"reason": err.Error()}}
}

func resolutionError(err error) *proto.Error {
	switch {
	case errors.Is(err, workspace.ErrWorkspaceNotFound):
		return &proto.Error{Code: proto.CodeWorkspaceNotFound, Message: err.Error()}
	case errors.Is(err, workspace.ErrCapabilityNotFound), errors.Is(err, workspace.ErrCapabilityAmbiguous), errors.Is(err, manifest.ErrRecipeNotFound):
		return &proto.Error{Code: proto.CodeCapabilityNotFound, Message: err.Error()}
	case errors.Is(err, manifest.ErrRecipeAmbiguous):
		return &proto.Error{Code: proto.CodeRecipeAmbiguous, Message: err.Error()}
	default:
		return internalError(err)
	}
}
