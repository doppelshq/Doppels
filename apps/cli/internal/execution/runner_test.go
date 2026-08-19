package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/runindex"
)

func TestExecuteShellSequentialIsolatedTypedReturnsAndArtifacts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "required.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := "do-not-persist-this-secret"
	capability := &manifest.Capability{
		TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Capability"},
		Metadata: manifest.Metadata{Name: "build", Version: "1.0.0"},
		Inputs:   map[string]manifest.InputContract{"name": {Type: "string", Required: true}},
		Outputs: map[string]manifest.OutputContract{
			"message": {Type: "string"}, "count": {Type: "integer"},
			"ratio": {Type: "number"}, "ok": {Type: "boolean"},
			"receipt":  {Type: "artifact", MediaType: "application/x-doppel-receipt"},
			"manifest": {Type: "artifact", MediaType: "application/json"},
		},
	}
	recipe := &manifest.Recipe{
		TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Recipe"},
		Metadata: manifest.Metadata{Name: "build-shell", Version: "1.0.0"},
		Provides: []string{"build"}, Runtime: "shell",
		Requires: &manifest.Requirements{Commands: []manifest.CommandRequirement{{Name: "sh"}}, HostEnv: []string{"MAPPED_SECRET", "HOST_SECRET"}, Files: []string{"required.txt"}},
		Env:      map[string]manifest.EnvironmentValue{"GLOBAL": literal("global")},
		Defaults: &manifest.Defaults{Approval: "never", Timeout: "5s"},
		Steps: []manifest.Step{
			{ID: "prepare", Name: "Prepare", Env: map[string]manifest.EnvironmentValue{
				"NAME":        literal("{{ inputs.name }}"),
				"SAFE_SECRET": hostRef("MAPPED_SECRET"),
			}, Run: &manifest.Run{Shell: "sh", Script: `
test "$GLOBAL" = global
test -z "${HOST_SECRET+x}"
printf '%s\n' "$SAFE_SECRET"
printf '%s\n' "$SAFE_SECRET" >&2
export MESSAGE="hello $NAME"
export COUNT=42
export RATIO=1.25e2
export LEAK=must-not-reach-next-step
printf '%s\n' "$MESSAGE" > receipt.txt
printf '{"name":"%s"}\n' "$NAME" > manifest.json
`}, Produces: map[string]manifest.Product{
				"message": {Env: "MESSAGE"}, "count": {Env: "COUNT"}, "ratio": {Env: "RATIO"},
				"receipt": {File: "receipt.txt"}, "manifest": {File: "manifest.json"},
			}},
			{ID: "verify", Name: "Verify", Approval: "required", Env: map[string]manifest.EnvironmentValue{
				"MESSAGE": literal("{{ steps.prepare.message }}"), "RECEIPT": literal("{{ steps.prepare.receipt }}"),
			}, Run: &manifest.Run{Shell: "sh", Script: `
test "$MESSAGE" = "hello Ada"
test -f "$RECEIPT"
test -z "${LEAK+x}"
export OK=true
`}, Produces: map[string]manifest.Product{"ok": {Env: "OK"}}},
		},
		Returns: manifest.ReturnsFrom(map[string]string{
			"message": "{{ steps.prepare.message }}", "count": "{{ steps.prepare.count }}",
			"ratio": "{{ steps.prepare.ratio }}", "receipt": "{{ steps.prepare.receipt }}",
			"manifest": "{{ steps.prepare.manifest }}", "ok": "{{ steps.verify.ok }}",
		}),
	}
	var approvals atomic.Int32
	var stdout, stderr bytes.Buffer
	inv := invocation(root, capability, recipe)
	inv.Inputs = map[string]any{"name": "Ada"}
	result, err := Execute(context.Background(), inv, Options{
		Environment: []string{"PATH=" + os.Getenv("PATH"), "MAPPED_SECRET=" + secret, "HOST_SECRET=not-inherited"},
		Approve: func(_ context.Context, request ApprovalRequest) (bool, error) {
			approvals.Add(1)
			return request.StepID == "verify", nil
		},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "succeeded" {
		t.Fatalf("status = %s", result.Status)
	}
	if approvals.Load() != 1 {
		t.Fatalf("approval calls = %d", approvals.Load())
	}
	if got := result.Returns["message"]; got != "hello Ada" {
		t.Errorf("message = %#v", got)
	}
	if got := result.Returns["count"]; got != int64(42) {
		t.Errorf("count = %#v (%T)", got, got)
	}
	if got := result.Returns["ratio"]; got != float64(125) {
		t.Errorf("ratio = %#v (%T)", got, got)
	}
	if got := result.Returns["ok"]; got != true {
		t.Errorf("ok = %#v", got)
	}
	if strings.Contains(stdout.String(), secret) || !strings.Contains(stdout.String(), "[REDACTED]") {
		t.Fatalf("stdout was not redacted: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "[REDACTED]") {
		t.Fatalf("stderr was not redacted: %q", stderr.String())
	}
	if len(result.Artifacts) != 2 {
		t.Fatalf("artifacts = %d", len(result.Artifacts))
	}
	receipt := result.Returns["receipt"].(ArtifactReference)
	if receipt.MediaType != "application/x-doppel-receipt" {
		t.Errorf("receipt media type = %s", receipt.MediaType)
	}
	if receipt.SizeBytes != int64(len("hello Ada\n")) || len(receipt.SHA256) != 64 {
		t.Errorf("receipt integrity = size %d sha %q", receipt.SizeBytes, receipt.SHA256)
	}
	if canonical := result.Artifacts[receipt.ID]; !reflect.DeepEqual(canonical, receipt) {
		t.Errorf("canonical artifact differs: %#v != %#v", canonical, receipt)
	}
	if data, err := os.ReadFile(receipt.LocalPath); err != nil || string(data) != "hello Ada\n" {
		t.Fatalf("receipt = %q, %v", data, err)
	}
	for _, name := range []string{"request.json", "run.json", "events.jsonl", filepath.Join("logs", "prepare.stdout.log")} {
		if _, err := os.Stat(filepath.Join(result.StateDir, name)); err != nil {
			t.Errorf("missing state %s: %v", name, err)
		}
	}
	logData, err := os.ReadFile(filepath.Join(result.StateDir, "logs", "prepare.stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), secret) || !strings.Contains(string(logData), "[REDACTED]") {
		t.Fatalf("stored log was not redacted: %q", logData)
	}
	stderrData, err := os.ReadFile(filepath.Join(result.StateDir, "logs", "prepare.stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stderrData), secret) || !strings.Contains(string(stderrData), "[REDACTED]") {
		t.Fatalf("stored stderr was not redacted: %q", stderrData)
	}
	assertEventTypes(t, result.Events, []string{"run_created", "validation_succeeded", "step_started", "step_succeeded", "approval_requested", "approval_approved", "step_started", "step_succeeded", "run_succeeded"})
}

func TestExecutePublishesRunEventsAndReturnsInDurableOrder(t *testing.T) {
	root := t.TempDir()
	var observed []string
	result, err := Execute(context.Background(), invocation(root, scalarCapability(), scalarRecipe("export VALUE=ok")), Options{
		Environment: []string{"PATH=" + os.Getenv("PATH")},
		OnRun: func(_ context.Context, run RunRecord) error {
			if run.ID == "" {
				t.Fatal("OnRun received an empty id")
			}
			observed = append(observed, "run")
			return nil
		},
		BeforeSuccess: func(_ context.Context, _ RunRecord, returns, evidence map[string]any) error {
			if returns["value"] != "ok" {
				t.Fatalf("returns = %#v", returns)
			}
			if evidence != nil {
				t.Fatalf("evidence = %#v", evidence)
			}
			observed = append(observed, "before-success")
			return nil
		},
		OnEvent: func(_ context.Context, event RunEvent) error {
			observed = append(observed, "event:"+event.Type)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"run", "event:run_created", "event:validation_succeeded",
		"event:step_started", "event:step_succeeded", "before-success",
		"event:run_succeeded",
	}
	if !reflect.DeepEqual(observed, want) {
		t.Fatalf("observed = %#v, want %#v", observed, want)
	}
	if result.Status != "succeeded" {
		t.Fatalf("status = %s", result.Status)
	}
}

func TestExecuteStopsBeforeStepsWhenRunCannotBePublished(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "must-not-run")
	result, err := Execute(context.Background(), invocation(root, scalarCapability(), scalarRecipe("touch must-not-run; export VALUE=ok")), Options{
		Environment: []string{"PATH=" + os.Getenv("PATH")},
		OnRun: func(context.Context, RunRecord) error {
			return errors.New("remote unavailable")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "publish Run") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Step ran before Run acknowledgement: %v", statErr)
	}
	if result.Status != "failed" {
		t.Fatalf("status = %s, want failed so index does not stick on running", result.Status)
	}
	assertIndexedStatus(t, root, result.Run.ID, "failed")
}

func TestExecuteIndexesSucceededEvenIfTerminalPublishFails(t *testing.T) {
	root := t.TempDir()
	result, err := Execute(context.Background(), invocation(root, scalarCapability(), scalarRecipe("export VALUE=ok")), Options{
		Environment: []string{"PATH=" + os.Getenv("PATH")},
		OnEvent: func(_ context.Context, event RunEvent) error {
			if event.Type == "run_succeeded" {
				return errors.New("channel closed")
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "publish RunEvent") {
		t.Fatalf("error = %v", err)
	}
	if result.Status != "succeeded" {
		t.Fatalf("status = %s", result.Status)
	}
	assertIndexedStatus(t, root, result.Run.ID, "succeeded")
}

func assertIndexedStatus(t *testing.T, root, runID, want string) {
	t.Helper()
	idx, err := runindex.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	record, err := idx.Get(runID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != want {
		t.Fatalf("index status = %q, want %q", record.Status, want)
	}
}

func TestExecuteFailsRequirementsBeforeStartingStep(t *testing.T) {
	root := t.TempDir()
	capability := scalarCapability()
	recipe := scalarRecipe(`export VALUE=ok`)
	recipe.Requires = &manifest.Requirements{Commands: []manifest.CommandRequirement{{Name: "command-that-cannot-exist-doppel"}}}
	result, err := Execute(context.Background(), invocation(root, capability, recipe), Options{Environment: []string{"PATH=" + os.Getenv("PATH")}})
	if !errors.Is(err, ErrRequirements) {
		t.Fatalf("error = %v", err)
	}
	assertEventTypes(t, result.Events, []string{"run_created", "validation_failed", "run_failed"})
}

func TestExecuteChecksHostEnvironmentAndFiles(t *testing.T) {
	tests := []struct {
		name     string
		requires manifest.Requirements
		env      []string
		contains string
	}{
		{name: "host env", requires: manifest.Requirements{HostEnv: []string{"NEEDED"}}, env: []string{"PATH=" + os.Getenv("PATH")}, contains: "NEEDED"},
		{name: "file", requires: manifest.Requirements{Files: []string{"missing.txt"}}, env: []string{"PATH=" + os.Getenv("PATH")}, contains: "missing.txt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recipe := scalarRecipe("export VALUE=ok")
			recipe.Requires = &test.requires
			result, err := Execute(context.Background(), invocation(t.TempDir(), scalarCapability(), recipe), Options{Environment: test.env})
			if !errors.Is(err, ErrRequirements) || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v", err)
			}
			assertEventTypes(t, result.Events, []string{"run_created", "validation_failed", "run_failed"})
		})
	}
}

func TestExecuteFailureAndTimeout(t *testing.T) {
	tests := []struct {
		name, script, timeout string
		target                error
	}{
		{name: "exit", script: "exit 7", target: ErrStepFailed},
		{name: "timeout", script: "sleep 2", timeout: "20ms", target: ErrStepTimedOut},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			recipe := scalarRecipe(test.script)
			recipe.Steps[0].Run.Timeout = test.timeout
			started := time.Now()
			result, err := Execute(context.Background(), invocation(root, scalarCapability(), recipe), Options{Environment: []string{"PATH=" + os.Getenv("PATH")}})
			if !errors.Is(err, test.target) {
				t.Fatalf("error = %v", err)
			}
			if test.name == "timeout" && time.Since(started) > time.Second {
				t.Fatalf("timeout left child alive for %s", time.Since(started))
			}
			assertEventTypes(t, result.Events, []string{"run_created", "validation_succeeded", "step_started", "step_failed", "run_failed"})
		})
	}
}

func TestExecuteInterruptionKillsStep(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "started")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	started := time.Now()
	result, err := Execute(ctx, invocation(root, scalarCapability(), scalarRecipe("touch started; sleep 2")), Options{Environment: []string{"PATH=" + os.Getenv("PATH")}})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("interruption left child alive for %s", time.Since(started))
	}
	assertEventTypes(t, result.Events, []string{"run_created", "validation_succeeded", "step_started", "step_failed", "run_interrupted"})
}

func TestExecuteApprovalRejectionCancelsRun(t *testing.T) {
	root := t.TempDir()
	recipe := scalarRecipe("export VALUE=ok")
	recipe.Defaults.Approval = "required"
	result, err := Execute(context.Background(), invocation(root, scalarCapability(), recipe), Options{
		Environment: []string{"PATH=" + os.Getenv("PATH")},
		Approve:     func(context.Context, ApprovalRequest) (bool, error) { return false, nil },
	})
	if !errors.Is(err, ErrApprovalRejected) || result.Status != "cancelled" {
		t.Fatalf("status/error = %s / %v", result.Status, err)
	}
	assertEventTypes(t, result.Events, []string{"run_created", "validation_succeeded", "approval_requested", "approval_rejected", "run_cancelled"})
}

func TestExecuteRejectsArtifactSymlinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "artifact.txt")); err != nil {
		t.Fatal(err)
	}
	capability := &manifest.Capability{TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Capability"}, Metadata: manifest.Metadata{Name: "artifact", Version: "1.0.0"}, Inputs: map[string]manifest.InputContract{}, Outputs: map[string]manifest.OutputContract{"file": {Type: "artifact"}}}
	recipe := &manifest.Recipe{TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Recipe"}, Metadata: manifest.Metadata{Name: "artifact", Version: "1.0.0"}, Provides: []string{"artifact"}, Runtime: "shell", Defaults: &manifest.Defaults{Approval: "never"}, Steps: []manifest.Step{{ID: "make", Name: "Make", Run: &manifest.Run{Shell: "sh", Script: "true"}, Produces: map[string]manifest.Product{"file": {File: "artifact.txt"}}}}, Returns: manifest.ReturnsFrom(map[string]string{"file": "{{ steps.make.file }}"})}
	result, err := Execute(context.Background(), invocation(root, capability, recipe), Options{Environment: []string{"PATH=" + os.Getenv("PATH")}})
	if !errors.Is(err, ErrStepFailed) || !strings.Contains(err.Error(), "symlink target escapes") {
		t.Fatalf("error = %v", err)
	}
	assertEventTypes(t, result.Events, []string{"run_created", "validation_succeeded", "step_started", "step_failed", "run_failed"})
}

func TestExecuteRejectsWorkingDirectoryTraversalAndScriptInterpolation(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*manifest.Recipe)
		contains string
	}{
		{name: "working directory", mutate: func(recipe *manifest.Recipe) { recipe.Steps[0].WorkingDirectory = "../outside" }, contains: "path escapes"},
		{name: "script expression", mutate: func(recipe *manifest.Recipe) { recipe.Steps[0].Run.Script = "echo {{ inputs.value }}" }, contains: "script contains"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recipe := scalarRecipe("export VALUE=ok")
			test.mutate(recipe)
			result, err := Execute(context.Background(), invocation(t.TempDir(), scalarCapability(), recipe), Options{Environment: []string{"PATH=" + os.Getenv("PATH")}})
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v", err)
			}
			if test.name == "script expression" {
				assertEventTypes(t, result.Events, []string{"run_created", "validation_failed", "run_failed"})
			}
		})
	}
}

func TestExecuteRejectsOneArtifactWithConflictingDeclaredMediaTypes(t *testing.T) {
	root := t.TempDir()
	capability := &manifest.Capability{TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Capability"}, Metadata: manifest.Metadata{Name: "media", Version: "1.0.0"}, Inputs: map[string]manifest.InputContract{}, Outputs: map[string]manifest.OutputContract{"first": {Type: "artifact", MediaType: "application/one"}, "second": {Type: "artifact", MediaType: "application/two"}}}
	recipe := &manifest.Recipe{TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Recipe"}, Metadata: manifest.Metadata{Name: "media", Version: "1.0.0"}, Provides: []string{"media"}, Runtime: "shell", Defaults: &manifest.Defaults{Approval: "never"}, Steps: []manifest.Step{{ID: "make", Name: "Make", Run: &manifest.Run{Shell: "sh", Script: "printf data > value.bin"}, Produces: map[string]manifest.Product{"file": {File: "value.bin"}}}}, Returns: manifest.ReturnsFrom(map[string]string{"first": "{{ steps.make.file }}", "second": "{{ steps.make.file }}"})}
	_, err := Execute(context.Background(), invocation(root, capability, recipe), Options{Environment: []string{"PATH=" + os.Getenv("PATH")}})
	if err == nil || !strings.Contains(err.Error(), "incompatible media types") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecuteManualRecipeAndNoRecipe(t *testing.T) {
	for _, withRecipe := range []bool{true, false} {
		t.Run(map[bool]string{true: "manual-recipe", false: "no-recipe"}[withRecipe], func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "procedure.md"), []byte("Do it"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "answer.txt"), []byte("answer"), 0o600); err != nil {
				t.Fatal(err)
			}
			capability := &manifest.Capability{TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Capability"}, Metadata: manifest.Metadata{Name: "manual", Version: "1.0.0"}, Inputs: map[string]manifest.InputContract{}, Outputs: map[string]manifest.OutputContract{"answer": {Type: "artifact", MediaType: "text/plain"}}}
			var recipe *manifest.Recipe
			if withRecipe {
				recipe = &manifest.Recipe{TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Recipe"}, Metadata: manifest.Metadata{Name: "manual", Version: "1.0.0"}, Provides: []string{"manual"}, Runtime: "manual", Procedure: &manifest.Procedure{Readme: "procedure.md"}, Evidence: map[string]manifest.Evidence{"notes": {Type: "string"}}}
			}
			inv := invocation(root, capability, recipe)
			inv.RecipeDirectory = root
			result, err := Execute(context.Background(), inv, Options{Manual: func(_ context.Context, request ManualRequest) (ManualResult, error) {
				expectedProcedure, _ := filepath.EvalSymlinks(filepath.Join(root, "procedure.md"))
				if withRecipe && request.ProcedurePath != expectedProcedure {
					t.Errorf("procedure = %q", request.ProcedurePath)
				}
				return ManualResult{Returns: map[string]any{"answer": FileValue{Path: "answer.txt"}}, Evidence: map[string]any{"notes": "verified"}}, nil
			}})
			if err != nil {
				t.Fatalf("Execute() = %v", err)
			}
			if result.Status != "succeeded" || len(result.Artifacts) != 1 {
				t.Fatalf("result = %#v", result)
			}
			assertEventTypes(t, result.Events, []string{"run_created", "validation_succeeded", "run_succeeded"})
		})
	}
}

func TestExecuteWithoutManualCallbackRemainsPending(t *testing.T) {
	root := t.TempDir()
	result, err := Execute(context.Background(), invocation(root, scalarCapability(), nil), Options{})
	if !errors.Is(err, ErrManualRequired) || result.Status != "pending_manual" {
		t.Fatalf("status/error = %s / %v", result.Status, err)
	}
	assertEventTypes(t, result.Events, []string{"run_created", "validation_succeeded"})
	assertIndexedStatus(t, root, result.Run.ID, "pending_manual")
}

func TestExistingRequestIsPreserved(t *testing.T) {
	root := t.TempDir()
	createdAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	inv := invocation(root, scalarCapability(), scalarRecipe("export VALUE=ok"))
	existing := RequestRecord{APIVersion: APIVersion, Kind: "Request", ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", CreatedAt: createdAt, IdempotencyKey: "share:1", Origin: "share", Capability: inv.CapabilityRef, Inputs: map[string]any{}, RequestedBy: ActorReference{Kind: "guest", ID: "guest-1"}, Space: "personal", ShareID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}
	inv.ExistingRequest = &existing
	inv.RequestedBy = ActorReference{}
	result, err := Execute(context.Background(), inv, Options{Environment: []string{"PATH=" + os.Getenv("PATH")}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Request, existing) {
		t.Fatalf("request changed:\n%#v\n%#v", result.Request, existing)
	}
	data, err := os.ReadFile(filepath.Join(result.StateDir, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted RequestRecord
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, existing) {
		t.Fatalf("persisted request changed: %#v", persisted)
	}
}

func TestExistingRequestMismatchIsRejected(t *testing.T) {
	inv := invocation(t.TempDir(), scalarCapability(), nil)
	existing := RequestRecord{APIVersion: APIVersion, Kind: "Request", ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", CreatedAt: time.Now().UTC(), IdempotencyKey: "share:1", Origin: "share", Capability: inv.CapabilityRef, Inputs: map[string]any{}, RequestedBy: ActorReference{Kind: "guest", ID: "guest-1"}, ShareID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}
	inv.ExistingRequest = &existing
	inv.ShareID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := Execute(context.Background(), inv, Options{}); err == nil || !strings.Contains(err.Error(), "shareId differs") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecuteNeverReusesRunID(t *testing.T) {
	root := t.TempDir()
	inv := invocation(root, scalarCapability(), scalarRecipe("export VALUE=ok"))
	inv.RunID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	options := Options{Environment: []string{"PATH=" + os.Getenv("PATH")}}
	if _, err := Execute(context.Background(), inv, options); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), inv, options); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second Execute error = %v", err)
	}
}

func TestCommandVersionCaretAndTildeRanges(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "tool")
	writeVersion := func(version string) {
		t.Helper()
		if err := os.WriteFile(tool, []byte("#!/bin/sh\necho tool "+version+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		version, constraint string
		matches             bool
	}{
		{version: "0.2.9", constraint: "^0.2.3", matches: true},
		{version: "0.3.0", constraint: "^0.2.3", matches: false},
		{version: "0.0.4", constraint: "^0.0.3", matches: false},
		{version: "1.4.9", constraint: "~1.4.0", matches: true},
		{version: "1.5.0", constraint: "~1.4.0", matches: false},
	}
	for _, test := range tests {
		writeVersion(test.version)
		err := checkCommandVersion(context.Background(), tool, test.constraint, []string{"PATH=" + os.Getenv("PATH")})
		if (err == nil) != test.matches {
			t.Errorf("%s %s: error = %v", test.version, test.constraint, err)
		}
	}
}

func TestParseInputsStrict(t *testing.T) {
	capability := &manifest.Capability{Inputs: map[string]manifest.InputContract{
		"count": {Type: "integer", Required: true}, "ratio": {Type: "number"},
		"enabled": {Type: "boolean", Default: true}, "mode": {Type: "string", Enum: []any{"safe", "fast"}},
	}}
	got, err := ParseInputs(capability, map[string]string{"count": "42", "ratio": "1e2", "mode": "safe"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"count": int64(42), "ratio": float64(100), "enabled": true, "mode": "safe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inputs = %#v", got)
	}
	for _, invalid := range []map[string]string{{"count": "4.2"}, {"count": "1", "enabled": "TRUE"}, {"count": "1", "ratio": "NaN"}} {
		if _, err := ParseInputs(capability, invalid); err == nil {
			t.Fatalf("accepted invalid inputs %#v", invalid)
		}
	}
	for _, boundary := range []string{"-9007199254740991", "9007199254740991"} {
		if _, err := ParseInputs(capability, map[string]string{"count": boundary}); err != nil {
			t.Fatalf("portable integer boundary %s: %v", boundary, err)
		}
	}
	for _, outside := range []string{"-9007199254740992", "9007199254740992"} {
		if _, err := ParseInputs(capability, map[string]string{"count": outside}); err == nil {
			t.Fatalf("accepted non-portable integer input %s", outside)
		}
		if _, err := parseOutputString(outside, "integer"); err == nil {
			t.Fatalf("accepted non-portable integer output %s", outside)
		}
	}
	for _, portable := range []string{"-9007199254740991", "1.5", "9007199254740991"} {
		if _, err := parseOutputString(portable, "number"); err != nil {
			t.Fatalf("portable number %s: %v", portable, err)
		}
	}
	for _, outside := range []string{"-9007199254740992", "9007199254740992", "1e20"} {
		if _, err := ParseInputs(capability, map[string]string{"count": "1", "ratio": outside}); err == nil {
			t.Fatalf("accepted non-portable integral number input %s", outside)
		}
		if _, err := parseOutputString(outside, "number"); err == nil {
			t.Fatalf("accepted non-portable integral number output %s", outside)
		}
	}
}

func TestExecuteLogStreamCap(t *testing.T) {
	root := t.TempDir()
	limit := 64
	capability := &manifest.Capability{
		TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Capability"},
		Metadata: manifest.Metadata{Name: "noise", Version: "1.0.0"},
		Inputs:   map[string]manifest.InputContract{},
		Outputs:  map[string]manifest.OutputContract{"value": {Type: "string"}},
	}
	recipe := &manifest.Recipe{
		TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Recipe"},
		Metadata: manifest.Metadata{Name: "noise-shell", Version: "1.0.0"},
		Provides: []string{"noise"}, Runtime: "shell",
		Defaults: &manifest.Defaults{Approval: "never", Timeout: "5s"},
		Steps: []manifest.Step{{
			ID: "run", Name: "Run",
			Run: &manifest.Run{Shell: "sh", Script: `
i=0
while [ "$i" -lt 200 ]; do
  printf A
  printf B >&2
  i=$((i+1))
done
export VALUE=ok
`},
			Produces: map[string]manifest.Product{"value": {Env: "VALUE"}},
		}},
		Returns: manifest.ReturnsFrom(map[string]string{"value": "{{ steps.run.value }}"}),
	}
	result, err := Execute(context.Background(), invocation(root, capability, recipe), Options{
		Environment:    []string{"PATH=" + os.Getenv("PATH")},
		LogStreamLimit: limit,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(result.Steps) != 1 || !result.Steps[0].Truncated {
		t.Fatalf("step truncated = %#v", result.Steps)
	}
	stdoutPath := filepath.Join(result.StateDir, result.Steps[0].StdoutPath)
	stdoutData, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(stdoutData, bytes.Repeat([]byte("A"), limit)) {
		t.Fatalf("stdout prefix = %q", stdoutData[:min(80, len(stdoutData))])
	}
	if !bytes.Contains(stdoutData, []byte("[doppels: truncated after 64B]")) {
		t.Fatalf("stdout missing marker: %q", stdoutData)
	}
	body := bytes.Split(stdoutData, []byte("\n[doppels:"))[0]
	if len(body) > limit {
		t.Fatalf("retained body len = %d, want <= %d", len(body), limit)
	}
	var truncatedEvent bool
	for _, event := range result.Events {
		if event.Type != "step_succeeded" {
			continue
		}
		if flag, _ := event.Data["truncated"].(bool); flag {
			truncatedEvent = true
		}
	}
	if !truncatedEvent {
		t.Fatal("expected truncated=true on step_succeeded")
	}
}

func scalarCapability() *manifest.Capability {
	return &manifest.Capability{TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Capability"}, Metadata: manifest.Metadata{Name: "scalar", Version: "1.0.0"}, Inputs: map[string]manifest.InputContract{}, Outputs: map[string]manifest.OutputContract{"value": {Type: "string"}}}
}

func scalarRecipe(script string) *manifest.Recipe {
	return &manifest.Recipe{TypeMeta: manifest.TypeMeta{APIVersion: APIVersion, Kind: "Recipe"}, Metadata: manifest.Metadata{Name: "scalar-shell", Version: "1.0.0"}, Provides: []string{"scalar"}, Runtime: "shell", Defaults: &manifest.Defaults{Approval: "never", Timeout: "3s"}, Steps: []manifest.Step{{ID: "run", Name: "Run", Run: &manifest.Run{Shell: "sh", Script: script}, Produces: map[string]manifest.Product{"value": {Env: "VALUE"}}}}, Returns: manifest.ReturnsFrom(map[string]string{"value": "{{ steps.run.value }}"})}
}

func invocation(root string, capability *manifest.Capability, recipe *manifest.Recipe) Invocation {
	capabilityRef := DefinitionReference{Name: capability.Metadata.Name, Version: capability.Metadata.Version, ManifestSHA256: strings.Repeat("a", 64), Schema: SchemaReference{ID: manifest.CapabilitySchemaID, SHA256: manifest.CapabilitySchemaSHA256}}
	invocation := Invocation{ProjectRoot: root, Capability: capability, CapabilityRef: capabilityRef, Inputs: map[string]any{}, RequestedBy: ActorReference{Kind: "identity", ID: "requester"}, Executor: ActorReference{Kind: "identity", ID: "executor"}}
	if recipe != nil {
		recipeRef := DefinitionReference{Name: recipe.Metadata.Name, Version: recipe.Metadata.Version, ManifestSHA256: strings.Repeat("c", 64), Schema: SchemaReference{ID: manifest.RecipeSchemaID, SHA256: manifest.RecipeSchemaSHA256}}
		invocation.Recipe, invocation.RecipeRef, invocation.NodeID = recipe, &recipeRef, "local"
	}
	return invocation
}

func literal(value string) manifest.EnvironmentValue {
	return manifest.EnvironmentValue{Literal: &value}
}
func hostRef(name string) manifest.EnvironmentValue {
	return manifest.EnvironmentValue{HostEnv: &manifest.HostEnvRef{From: "host_env", Name: name}}
}

func assertEventTypes(t *testing.T, events []RunEvent, expected []string) {
	t.Helper()
	actual := make([]string, len(events))
	for index, event := range events {
		actual[index] = event.Type
		if event.Sequence != index {
			t.Errorf("event %d sequence = %d", index, event.Sequence)
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("event types = %#v; want %#v", actual, expected)
	}
}
