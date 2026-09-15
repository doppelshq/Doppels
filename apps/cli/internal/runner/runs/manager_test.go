package runs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/runindex"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/workspace"
	"doppels.so/cli/internal/runstate"
)

// TestSourceForClientRecognizesOfficialHandshakeNames reproduces review
// finding 11: the RFC's own handshake example (docs/runner-protocol.md §6)
// sends client.name "doppels-desktop", and this codebase's established CLI
// User-Agent convention (internal/shareclient, internal/registryclient) is
// "doppels-cli" — but sourceForClient only recognized the bare "cli"/
// "desktop" strings. Every real CLI- or Desktop-originated Run fell through
// to "local", the same source recorded for a local-only client that never
// identified itself at all.
func TestSourceForClientRecognizesOfficialHandshakeNames(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"doppels-cli", "cli"},
		{"doppels-desktop", "desktop"},
		{"cli", "cli"},
		{"desktop", "desktop"},
		{"DOPPELS-CLI", "cli"},
		{"", "local"},
		{"something-else", "local"},
	}
	for _, tt := range tests {
		if got := sourceForClient(tt.name); got != tt.want {
			t.Errorf("sourceForClient(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestStartIsAsyncPersistedAndIdempotentAcrossRestart(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := NewManager(ctx, service, Config{
		NodeID:      "node-test",
		Environment: []string{"PATH=" + os.Getenv("PATH")},
	})

	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"same"}`)
	const callers = 20
	results := make(chan StartResult, callers)
	errs := make(chan *proto.Error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func() {
			ready.Done()
			<-start
			result, rpcErr := manager.Start("desktop", params)
			results <- result
			errs <- rpcErr
		}()
	}
	ready.Wait()
	close(start)
	var original StartResult
	for i := 0; i < callers; i++ {
		got := <-results
		if rpcErr := <-errs; rpcErr != nil {
			t.Fatalf("Start error = %+v", rpcErr)
		}
		if original.RunID == "" {
			original = got
		}
		if got != original {
			t.Fatalf("Start result = %#v, want %#v", got, original)
		}
	}
	waitForStatus(t, root, original.RunID, "succeeded")
	data, err := os.ReadFile(filepath.Join(root, "executions.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "executed") != 1 {
		t.Fatalf("script executions = %q, want exactly one", data)
	}
	idx, err := runindex.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	record, err := idx.Get(original.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Source != "desktop" || record.NodeID != "node-test" || record.FinishedAt == "" {
		t.Fatalf("indexed Run = %#v", record)
	}
	idx.Close()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := NewManager(ctx, service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer restarted.Close()
	retryParams := []byte(`{"workspace":` + quote(root) + `,"capability":"greet@1.0.0","inputs":{"count":1.0},"approvalMode":"auto","idempotencyKey":"same"}`)
	retry, rpcErr := restarted.Start("cli", retryParams)
	if rpcErr != nil || retry != original {
		t.Fatalf("restart retry = %#v, %+v; want %#v", retry, rpcErr, original)
	}
	collision := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":2},"approvalMode":"auto","idempotencyKey":"same"}`)
	if _, rpcErr := restarted.Start("cli", collision); rpcErr == nil || rpcErr.Code != proto.CodeInvalidParams {
		t.Fatalf("collision error = %+v, want -32602", rpcErr)
	}
}

func TestStartRejectsInvalidPayloadBeforeReservation(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	tests := []struct {
		name   string
		params []byte
		code   int
	}{
		{name: "missing required input", params: []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{},"approvalMode":"auto","idempotencyKey":"free"}`), code: proto.CodeInvalidInputs},
		{name: "duplicate input key", params: []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1,"count":2},"approvalMode":"auto","idempotencyKey":"dup"}`), code: proto.CodeInvalidParams},
		{name: "unbounded numeric expansion", params: []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1e100000},"approvalMode":"auto","idempotencyKey":"huge"}`), code: proto.CodeInvalidParams},
		{name: "unknown capability", params: []byte(`{"workspace":` + quote(root) + `,"capability":"missing","inputs":{},"approvalMode":"auto","idempotencyKey":"missing"}`), code: proto.CodeCapabilityNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, rpcErr := manager.Start("cli", tt.params); rpcErr == nil || rpcErr.Code != tt.code {
				t.Fatalf("error = %+v, want %d", rpcErr, tt.code)
			}
		})
	}

	valid := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":3},"approvalMode":"auto","idempotencyKey":"free"}`)
	result, rpcErr := manager.Start("unknown-client", valid)
	if rpcErr != nil {
		t.Fatalf("valid Start after rejected reservation: %+v", rpcErr)
	}
	waitForStatus(t, root, result.RunID, "succeeded")
	idx, err := runindex.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	record, err := idx.Get(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Source != "local" {
		t.Fatalf("fallback source = %q, want local", record.Source)
	}
}

func TestStartWithoutRecipeBecomesDurablePendingManual(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()
	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"interactive","idempotencyKey":"manual"}`)
	result, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForStatus(t, root, result.RunID, "pending_manual")
	if manager.Active() != 0 {
		t.Fatalf("active executions = %d, want 0 after pendingManual", manager.Active())
	}
}

func TestCancelIsIdempotentAndWritesOneTerminal(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	// Replace the quick Recipe with one that stays inside a process group
	// until cancelRun interrupts it.
	writeRunFixture(t, filepath.Join(root, ".doppels", "recipes", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: greet-shell, version: 1.0.0}
provides: [greet]
runtime: shell
defaults: {approval: never}
steps:
  - id: run
    name: Run
    run: {shell: sh, script: "touch started; sleep 30; export VALUE=ok"}
    produces: {value: {env: VALUE}}
returns: {value: "{{ steps.run.value }}"}
`)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()
	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"cancel"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForFile(t, filepath.Join(root, "started"))

	status, rpcErr := manager.Cancel(started.RunID, "line one\nline two")
	if rpcErr != nil || status != "interrupted" {
		t.Fatalf("Cancel = %q, %+v", status, rpcErr)
	}
	status, rpcErr = manager.Cancel(started.RunID, "again")
	if rpcErr != nil || status != "interrupted" {
		t.Fatalf("idempotent Cancel = %q, %+v", status, rpcErr)
	}
	detail, err := runstate.Load(root, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, event := range detail.Events {
		if strings.HasPrefix(event.Type, "run_") && event.Type != "run_created" {
			if event.Type == "run_succeeded" || event.Type == "run_failed" || event.Type == "run_cancelled" || event.Type == "run_interrupted" {
				terminals++
			}
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d; events = %#v", terminals, detail.Events)
	}
	if _, rpcErr := manager.Cancel("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ""); rpcErr == nil || rpcErr.Code != proto.CodeRunNotFound {
		t.Fatalf("unknown run error = %+v", rpcErr)
	}
}

func TestCancelPendingManualMaterializesCancelled(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()
	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"interactive","idempotencyKey":"manual-cancel"}`)
	started, rpcErr := manager.Start("desktop", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForStatus(t, root, started.RunID, "pending_manual")
	status, rpcErr := manager.Cancel(started.RunID, "no longer needed")
	if rpcErr != nil || status != "cancelled" {
		t.Fatalf("Cancel pendingManual = %q, %+v", status, rpcErr)
	}
	detail, err := runstate.Load(root, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got := detail.Events[len(detail.Events)-1]; got.Type != "run_cancelled" || got.Sequence != len(detail.Events)-1 {
		t.Fatalf("last event = %#v", got)
	}
}

func runnerWorkspace(t *testing.T, withRecipe bool) (*workspace.Service, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	writeRunFixture(t, filepath.Join(root, ".doppels", "capabilities", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: greet, version: 1.0.0}
inputs:
  count: {type: integer, required: true}
outputs:
  value: {type: string}
`)
	if withRecipe {
		writeRunFixture(t, filepath.Join(root, ".doppels", "recipes", "greet.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: greet-shell, version: 1.0.0}
provides: [greet]
runtime: shell
defaults: {approval: never}
steps:
  - id: run
    name: Run
    run:
      shell: sh
      script: |
        printf executed >> executions.txt
        export VALUE="ok:$DOPPEL_INPUT_COUNT"
    produces:
      value: {env: VALUE}
returns:
  value: "{{ steps.run.value }}"
`)
	}
	registry := workspace.NewRegistry(filepath.Join(base, "workspaces.json"))
	service := workspace.NewService(registry, workspace.Deps{Host: manifest.OSHost{}})
	if _, _, err := service.AddWorkspace(root); err != nil {
		t.Fatal(err)
	}
	return service, root
}

func writeRunFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func quote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func waitForStatus(t *testing.T, root, runID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		idx, err := runindex.Open(root)
		if err == nil {
			record, getErr := idx.Get(runID)
			idx.Close()
			if getErr == nil && record.Status == want {
				return
			}
			if getErr != nil && !errors.Is(getErr, os.ErrNotExist) {
				// The row can legitimately be between reservation and first update.
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Run %s did not reach status %s", runID, want)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s was not created", path)
}
