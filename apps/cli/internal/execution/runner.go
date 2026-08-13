package execution

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"doppels.so/cli/internal/localstate"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/runindex"
)

type runner struct {
	ctx        context.Context
	invocation Invocation
	options    Options
	store      *localstate.Store
	result     Result
	inputs     map[string]any
	now        func() time.Time
	sequence   int
}

func Execute(ctx context.Context, invocation Invocation, options Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if invocation.ExistingRequest != nil {
		if err := validateExistingRequest(invocation); err != nil {
			return Result{}, err
		}
	}
	inputsForValidation := invocation.Inputs
	if invocation.ExistingRequest != nil {
		inputsForValidation = invocation.ExistingRequest.Inputs
		invocation.CapabilityRef = invocation.ExistingRequest.Capability
		invocation.RequestedBy = invocation.ExistingRequest.RequestedBy
	}
	invocation.Inputs = inputsForValidation
	inputs, err := validateInvocation(invocation)
	if err != nil {
		return Result{}, err
	}

	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	requestID := invocation.RequestID
	if invocation.ExistingRequest != nil {
		requestID = invocation.ExistingRequest.ID
	}
	if requestID == "" {
		requestID, err = newUUID()
		if err != nil {
			return Result{}, fmt.Errorf("create Request id: %w", err)
		}
	}
	runID := invocation.RunID
	if runID == "" {
		runID, err = newUUID()
		if err != nil {
			return Result{}, fmt.Errorf("create Run id: %w", err)
		}
	}
	store, err := localstate.Open(invocation.ProjectRoot, runID)
	if err != nil {
		return Result{}, err
	}

	r := &runner{ctx: ctx, invocation: invocation, options: options, store: store, inputs: inputs, now: now}
	if err := r.initialize(requestID, runID); err != nil {
		return r.result, err
	}
	if options.OnRun != nil {
		if err := options.OnRun(ctx, r.result.Run); err != nil {
			return r.sealAfterError(fmt.Errorf("publish Run: %w", err))
		}
	}
	if err := r.emit("run_created", "", nil); err != nil {
		return r.sealAfterError(err)
	}
	if err := preflight(ctx, invocation, options, inputs); err != nil {
		if ctx.Err() != nil {
			return r.interrupt(ctx.Err())
		}
		if emitErr := r.emit("validation_failed", "", map[string]any{"error": err.Error()}); emitErr != nil {
			return r.sealAfterError(emitErr)
		}
		return r.fail(err)
	}
	if err := r.emit("validation_succeeded", "", nil); err != nil {
		return r.sealAfterError(err)
	}

	if invocation.Recipe == nil || invocation.Recipe.Runtime == "manual" {
		return r.runManual(ctx)
	}
	return r.runShell(ctx)
}

func (r *runner) initialize(requestID, runID string) error {
	createdAt := r.invocation.RequestCreatedAt
	if createdAt.IsZero() {
		createdAt = r.now()
	}
	request := RequestRecord{
		APIVersion: APIVersion, Kind: "Request", ID: requestID, CreatedAt: createdAt,
		IdempotencyKey: r.invocation.IdempotencyKey, Capability: r.invocation.CapabilityRef,
		Inputs: cloneMap(r.inputs), RequestedBy: r.invocation.RequestedBy,
		AssignedTo: r.invocation.AssignedTo, Space: r.invocation.Space, ShareID: r.invocation.ShareID,
	}
	if request.ShareID != "" {
		request.Origin = "share"
	} else {
		request.Origin = "cli"
	}
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = "local:" + requestID
	}
	if existing := r.invocation.ExistingRequest; existing != nil {
		request = *existing
		request.Inputs = cloneMap(existing.Inputs)
	}
	run := RunRecord{
		APIVersion: APIVersion, Kind: "Run", ID: runID, RequestID: request.ID,
		CreatedAt: r.now(), Capability: request.Capability, Inputs: cloneMap(r.inputs),
		Executor: r.invocation.Executor,
	}
	if r.invocation.Recipe != nil {
		run.Recipe = r.invocation.RecipeRef
	}
	if r.invocation.Recipe != nil && r.invocation.Recipe.Runtime == "shell" {
		run.NodeID = r.invocation.NodeID
	}
	r.result = Result{Status: "running", StateDir: r.store.Dir(), Request: request, Run: run, Artifacts: map[string]ArtifactReference{}}
	if err := r.store.WriteRequest(request); err != nil {
		return err
	}
	if err := r.store.WriteRun(run); err != nil {
		return err
	}
	return r.indexRun("running", false)
}

func (r *runner) emit(eventType, stepID string, data map[string]any) error {
	event := RunEvent{APIVersion: APIVersion, Kind: "RunEvent", RunID: r.result.Run.ID, Sequence: r.sequence, OccurredAt: r.now(), Type: eventType, StepID: stepID, Data: data}
	if err := r.store.AppendEvent(event); err != nil {
		return err
	}
	r.sequence++
	r.result.Events = append(r.result.Events, event)
	// Index terminal status as soon as the event is durable, before optional
	// remote publish. Publish failure must not leave the local index on "running".
	switch eventType {
	case "run_succeeded", "run_failed", "run_cancelled", "run_interrupted":
		status := strings.TrimPrefix(eventType, "run_")
		if err := r.indexRun(status, true); err != nil {
			return err
		}
	}
	if r.options.OnEvent != nil {
		if err := r.options.OnEvent(r.ctx, event); err != nil {
			return fmt.Errorf("publish RunEvent %d: %w", event.Sequence, err)
		}
	}
	return nil
}

func (r *runner) indexRun(status string, enqueueOutbox bool) error {
	idx, err := runindex.Open(r.invocation.ProjectRoot)
	if err != nil {
		return fmt.Errorf("open run index: %w", err)
	}
	defer idx.Close()
	recipe := ""
	if r.result.Run.Recipe != nil {
		recipe = r.result.Run.Recipe.Name + "@" + r.result.Run.Recipe.Version
	}
	record := runindex.Record{
		ID: r.result.Run.ID, RequestID: r.result.Run.RequestID, Status: status,
		Source: runindex.SourceLocal, Capability: r.result.Run.Capability.Name + "@" + r.result.Run.Capability.Version,
		Recipe: recipe, CreatedAt: r.result.Run.CreatedAt.UTC().Format(time.RFC3339Nano),
		StateDir: r.result.StateDir, SyncStatus: runindex.SyncNone,
	}
	if err := idx.Upsert(record); err != nil {
		return fmt.Errorf("index Run: %w", err)
	}
	if enqueueOutbox {
		if err := idx.EnqueueOutbox(record.ID, map[string]any{
			"id": record.ID, "requestId": record.RequestID, "status": record.Status,
			"capability": record.Capability, "recipe": record.Recipe, "createdAt": record.CreatedAt,
		}); err != nil {
			return fmt.Errorf("enqueue run sync: %w", err)
		}
	}
	return nil
}

func isTerminalStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "interrupted", "pending_manual":
		return true
	default:
		return false
	}
}

// sealAfterError guarantees the SQLite index leaves "running" when Execute
// aborts without a clean terminal path (publish errors, mid-run emit failures).
func (r *runner) sealAfterError(cause error) (Result, error) {
	if r.result.Run.ID == "" || isTerminalStatus(r.result.Status) {
		return r.result, cause
	}
	return r.fail(cause)
}

func (r *runner) fail(cause error) (Result, error) {
	if r.result.Status == "failed" && hasTerminalRunEvent(r.result.Events) {
		_ = r.indexRun("failed", true)
		return r.result, cause
	}
	r.result.Status = "failed"
	if err := r.emit("run_failed", "", map[string]any{"error": cause.Error()}); err != nil {
		_ = r.indexRun("failed", true)
		return r.result, cause
	}
	return r.result, cause
}

func (r *runner) interrupt(cause error) (Result, error) {
	if r.result.Status == "interrupted" && hasTerminalRunEvent(r.result.Events) {
		_ = r.indexRun("interrupted", true)
		return r.result, fmt.Errorf("%w: %v", ErrInterrupted, cause)
	}
	r.result.Status = "interrupted"
	if err := r.emit("run_interrupted", "", nil); err != nil {
		_ = r.indexRun("interrupted", true)
		return r.result, fmt.Errorf("%w: %v", ErrInterrupted, cause)
	}
	return r.result, fmt.Errorf("%w: %v", ErrInterrupted, cause)
}

func hasTerminalRunEvent(events []RunEvent) bool {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case "run_succeeded", "run_failed", "run_cancelled", "run_interrupted":
			return true
		}
	}
	return false
}

func (r *runner) runShell(ctx context.Context) (Result, error) {
	available := values{inputs: r.inputs, steps: make(map[string]map[string]any)}
	base := r.options.Environment
	if base == nil {
		base = os.Environ()
	}
	host := environmentMap(base)
	global, globalSecrets, err := resolveEnvironment(r.invocation.Recipe.Env, host, available)
	if err != nil {
		return r.fail(err)
	}

	for _, step := range r.invocation.Recipe.Steps {
		if ctx.Err() != nil {
			return r.interrupt(ctx.Err())
		}
		approval := step.Approval
		if approval == "" && r.invocation.Recipe.Defaults != nil {
			approval = r.invocation.Recipe.Defaults.Approval
		}
		if approval == "required" {
			if err := r.emit("approval_requested", step.ID, nil); err != nil {
				return r.sealAfterError(err)
			}
			approved := r.options.ApproveAll
			if !approved && r.options.Approve != nil {
				approved, err = r.options.Approve(ctx, ApprovalRequest{RunID: r.result.Run.ID, StepID: step.ID, Name: step.Name})
				if err != nil {
					if ctx.Err() != nil {
						return r.interrupt(ctx.Err())
					}
					return r.fail(fmt.Errorf("approval for Step %q: %w", step.ID, err))
				}
			}
			if !approved {
				if err := r.emit("approval_rejected", step.ID, nil); err != nil {
					return r.sealAfterError(err)
				}
				r.result.Status = "cancelled"
				if err := r.emit("run_cancelled", "", nil); err != nil {
					return r.sealAfterError(err)
				}
				return r.result, ErrApprovalRejected
			}
			if err := r.emit("approval_approved", step.ID, nil); err != nil {
				return r.sealAfterError(err)
			}
		} else if approval != "never" {
			return r.fail(fmt.Errorf("%w: Step %q has unresolved approval", ErrInvalidInvocation, step.ID))
		}

		workingDirectory, err := r.workingDirectory(step, available)
		if err != nil {
			return r.fail(err)
		}
		stepEnvironment, stepSecrets, err := resolveEnvironment(step.Env, host, available)
		if err != nil {
			return r.fail(err)
		}
		environment := minimalEnvironment(host)
		merge(environment, global)
		merge(environment, stepEnvironment)
		secrets := append(append([]string{}, globalSecrets...), stepSecrets...)

		if err := r.emit("step_started", step.ID, nil); err != nil {
			return r.sealAfterError(err)
		}
		stepResult, snapshot, runErr := r.executeStep(ctx, step, workingDirectory, environment, secrets)
		r.result.Steps = append(r.result.Steps, stepResult)
		if runErr != nil {
			data := map[string]any{"exitCode": stepResult.ExitCode, "stdout": stepResult.StdoutPath, "stderr": stepResult.StderrPath}
			if stepResult.Truncated {
				data["truncated"] = true
			}
			if errors.Is(runErr, ErrStepTimedOut) {
				data["timedOut"] = true
			}
			if err := r.emit("step_failed", step.ID, data); err != nil {
				return r.sealAfterError(err)
			}
			if errors.Is(runErr, ErrInterrupted) {
				return r.interrupt(runErr)
			}
			return r.fail(runErr)
		}
		products, err := collectProducts(r.store, r.invocation.ProjectRoot, workingDirectory, step, available, snapshot, r.result.Artifacts)
		if err != nil {
			failData := map[string]any{"error": err.Error(), "stdout": stepResult.StdoutPath, "stderr": stepResult.StderrPath}
			if stepResult.Truncated {
				failData["truncated"] = true
			}
			if emitErr := r.emit("step_failed", step.ID, failData); emitErr != nil {
				return r.sealAfterError(emitErr)
			}
			return r.fail(fmt.Errorf("%w: %v", ErrStepFailed, err))
		}
		available.steps[step.ID] = products
		r.result.Steps[len(r.result.Steps)-1].Products = products
		okData := map[string]any{"exitCode": 0, "stdout": stepResult.StdoutPath, "stderr": stepResult.StderrPath, "products": publicReturns(products)}
		if stepResult.Truncated {
			okData["truncated"] = true
		}
		if err := r.emit("step_succeeded", step.ID, okData); err != nil {
			return r.sealAfterError(err)
		}
	}

	returns, err := materializeShellReturns(r.invocation.Capability, r.invocation.Recipe, available, r.result.Artifacts)
	if err != nil {
		return r.fail(fmt.Errorf("%w: %v", ErrStepFailed, err))
	}
	r.result.Returns = returns
	if r.options.BeforeSuccess != nil {
		if err := r.options.BeforeSuccess(ctx, r.result.Run, returns, nil); err != nil {
			return r.fail(fmt.Errorf("publish successful returns: %w", err))
		}
	}
	r.result.Status = "succeeded"
	if err := r.emit("run_succeeded", "", map[string]any{"returns": publicReturns(returns)}); err != nil {
		return r.sealAfterError(err)
	}
	return r.result, nil
}

func (r *runner) workingDirectory(step manifest.Step, available values) (string, error) {
	relative := "."
	if r.invocation.Recipe.Defaults != nil && r.invocation.Recipe.Defaults.WorkingDirectory != "" {
		relative = r.invocation.Recipe.Defaults.WorkingDirectory
	}
	if step.WorkingDirectory != "" {
		relative = step.WorkingDirectory
	}
	rendered, err := renderTemplate(relative, available)
	if err != nil {
		return "", fmt.Errorf("Step %q workingDirectory: %w", step.ID, err)
	}
	path, err := securePath(r.invocation.ProjectRoot, filepath.Join(r.invocation.ProjectRoot, filepath.FromSlash(rendered)), true)
	if err != nil {
		return "", fmt.Errorf("Step %q workingDirectory: %w", step.ID, err)
	}
	return path, nil
}

func (r *runner) executeStep(ctx context.Context, step manifest.Step, workingDirectory string, environment map[string]string, secrets []string) (StepResult, map[string]string, error) {
	result := StepResult{ID: step.ID}
	timeoutText := step.Run.Timeout
	if timeoutText == "" && r.invocation.Recipe.Defaults != nil {
		timeoutText = r.invocation.Recipe.Defaults.Timeout
	}
	stepContext := ctx
	cancel := func() {}
	if timeoutText != "" {
		timeout, err := time.ParseDuration(timeoutText)
		if err != nil {
			return result, nil, fmt.Errorf("%w: Step %q timeout: %v", ErrInvalidInvocation, step.ID, err)
		}
		stepContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	snapshotFile, err := os.CreateTemp(r.store.Dir(), ".env-*")
	if err != nil {
		return result, nil, err
	}
	snapshotPath := snapshotFile.Name()
	snapshotFile.Close()
	os.Remove(snapshotPath)
	defer os.Remove(snapshotPath)
	scriptFile, err := os.CreateTemp(r.store.Dir(), ".step-*.sh")
	if err != nil {
		return result, nil, err
	}
	scriptPath := scriptFile.Name()
	defer os.Remove(scriptPath)
	wrapper := "__doppel_snapshot() { __doppel_status=$?; env -0 > \"$DOPPELS_ENV_SNAPSHOT\"; trap - 0; exit \"$__doppel_status\"; }\ntrap '__doppel_snapshot' 0\n" + step.Run.Script + "\n"
	if _, err := io.WriteString(scriptFile, wrapper); err != nil {
		scriptFile.Close()
		return result, nil, err
	}
	if err := scriptFile.Close(); err != nil {
		return result, nil, err
	}
	environment["DOPPELS_ENV_SNAPSHOT"] = snapshotPath

	shell, err := r.lookupCommand(step.Run.Shell, environment)
	if err != nil {
		return result, nil, fmt.Errorf("%w: shell %q: %v", ErrRequirements, step.Run.Shell, err)
	}
	command := exec.CommandContext(stepContext, shell, scriptPath)
	configureProcess(command)
	command.Dir = workingDirectory
	command.Env = environmentList(environment)
	limit := r.options.LogStreamLimit
	if limit <= 0 {
		limit = DefaultLogStreamLimit
	}
	stdout := newCappedWriter(limit)
	stderr := newCappedWriter(limit)
	command.Stdout, command.Stderr = stdout, stderr
	started := time.Now()
	runErr := command.Run()
	result.Duration = time.Since(started)
	result.Truncated = stdout.Truncated() || stderr.Truncated()
	redactedStdout := finalizeLogBytes(redact(stdout.Bytes(), secrets), stdout.Truncated(), limit)
	redactedStderr := finalizeLogBytes(redact(stderr.Bytes(), secrets), stderr.Truncated(), limit)
	result.StdoutPath, err = r.store.WriteLog(step.ID, "stdout", redactedStdout)
	if err != nil {
		return result, nil, err
	}
	result.StderrPath, err = r.store.WriteLog(step.ID, "stderr", redactedStderr)
	if err != nil {
		return result, nil, err
	}
	if r.options.Stdout != nil {
		_, _ = r.options.Stdout.Write(redactedStdout)
	}
	if r.options.Stderr != nil {
		_, _ = r.options.Stderr.Write(redactedStderr)
	}
	if runErr != nil {
		result.ExitCode = -1
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			result.ExitCode = exitError.ExitCode()
		}
		if ctx.Err() != nil {
			return result, nil, fmt.Errorf("%w: %v", ErrInterrupted, ctx.Err())
		}
		if errors.Is(stepContext.Err(), context.DeadlineExceeded) {
			return result, nil, fmt.Errorf("%w: Step %q exceeded %s", ErrStepTimedOut, step.ID, timeoutText)
		}
		return result, nil, fmt.Errorf("%w: Step %q exited with %d", ErrStepFailed, step.ID, result.ExitCode)
	}
	snapshot, err := readEnvironmentSnapshot(snapshotPath)
	if err != nil {
		return result, nil, fmt.Errorf("%w: Step %q did not leave an environment snapshot: %v", ErrStepFailed, step.ID, err)
	}
	return result, snapshot, nil
}

func (r *runner) lookupCommand(name string, environment map[string]string) (string, error) {
	if r.options.LookupCommand != nil {
		return r.options.LookupCommand(name)
	}
	return lookupPath(name, environment["PATH"])
}

func resolveEnvironment(spec map[string]manifest.EnvironmentValue, host map[string]string, available values) (map[string]string, []string, error) {
	result := make(map[string]string, len(spec))
	var secrets []string
	for name, value := range spec {
		if value.Literal != nil {
			rendered, err := renderTemplate(*value.Literal, available)
			if err != nil {
				return nil, nil, fmt.Errorf("environment %s: %w", name, err)
			}
			result[name] = rendered
			continue
		}
		if value.HostEnv == nil || value.HostEnv.From != "host_env" {
			return nil, nil, fmt.Errorf("environment %s has an invalid source", name)
		}
		hostValue, exists := host[value.HostEnv.Name]
		if !exists {
			return nil, nil, fmt.Errorf("required host environment variable %q is not set", value.HostEnv.Name)
		}
		result[name] = hostValue
		if hostValue != "" {
			secrets = append(secrets, hostValue)
		}
	}
	return result, secrets, nil
}

func minimalEnvironment(host map[string]string) map[string]string {
	result := map[string]string{}
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE"} {
		if value, exists := host[name]; exists {
			result[name] = value
		}
	}
	return result
}

func merge(target, source map[string]string) {
	for name, value := range source {
		target[name] = value
	}
}

func readEnvironmentSnapshot(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, entry := range bytes.Split(data, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		name, value, exists := bytes.Cut(entry, []byte{'='})
		if exists {
			result[string(name)] = string(value)
		}
	}
	return result, nil
}

func redact(data []byte, secrets []string) []byte {
	result := append([]byte(nil), data...)
	secrets = append([]string(nil), secrets...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret != "" {
			result = bytes.ReplaceAll(result, []byte(secret), []byte("[REDACTED]"))
		}
	}
	return result
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func newUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	hex := fmt.Sprintf("%x", value)
	return strings.Join([]string{hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32]}, "-"), nil
}
