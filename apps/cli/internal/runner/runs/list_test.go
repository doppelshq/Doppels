package runs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runner/workspace"
)

func startAndFinish(t *testing.T, manager *Manager, root, key string) StartResult {
	t.Helper()
	params := []byte(`{"workspace":` + quote(root) + `,"capability":"greet","inputs":{"count":1},"approvalMode":"auto","idempotencyKey":"` + key + `"}`)
	started, rpcErr := manager.Start("cli", params)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	waitForStatus(t, root, started.RunID, "succeeded")
	return started
}

func TestListRunsWithinWorkspacePaginatesAndFilters(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	first := startAndFinish(t, manager, root, "list-1")
	second := startAndFinish(t, manager, root, "list-2")

	page, rpcErr := manager.ListRuns(ListParams{Workspace: root, Limit: 1})
	if rpcErr != nil {
		t.Fatalf("ListRuns: %+v", rpcErr)
	}
	if len(page.Runs) != 1 || page.NextCursor == "" {
		t.Fatalf("first page = %#v", page)
	}
	next, rpcErr := manager.ListRuns(ListParams{Workspace: root, Limit: 1, Cursor: page.NextCursor})
	if rpcErr != nil {
		t.Fatalf("ListRuns page 2: %+v", rpcErr)
	}
	if len(next.Runs) != 1 || next.NextCursor != "" {
		t.Fatalf("second page = %#v", next)
	}
	seen := map[string]bool{page.Runs[0].RunID: true, next.Runs[0].RunID: true}
	if len(seen) != 2 || !seen[first.RunID] || !seen[second.RunID] {
		t.Fatalf("paginated Run IDs = %#v, want exactly %q and %q", seen, first.RunID, second.RunID)
	}

	if _, rpcErr := manager.ListRuns(ListParams{Workspace: "/does/not/exist"}); rpcErr == nil || rpcErr.Code != proto.CodeWorkspaceNotFound {
		t.Fatalf("unknown workspace error = %+v, want -32003", rpcErr)
	}
}

// TestListRunsRejectsCursorReferencingAnUnregisteredRoot reproduces review
// finding 6: the cross-workspace cursor is client-supplied and opaque, but
// its "roots" map was trusted as authoritative — listRuns called m.index()
// (which MkdirAll's a .doppels directory) on whatever path a cursor named,
// registered or not. A forged cursor must be rejected as a domain/params
// error before any filesystem mutation, never silently create state outside
// the registry.
// TestListRunsStatusAcceptsWireEnumAndRejectsUnknownValues reproduces
// review finding 9: listRuns's Status filter was passed straight through
// to runindex without any mapping or validation. The wire enum for a
// pendingManual Run is "pendingManual" (RunSummary.status, RFC §8), but
// storage uses runindex's "pending_manual" — filtering by the documented
// wire value silently matched nothing. An unrecognized status string was
// also silently accepted, returning an empty page instead of -32602.
func TestListRunsStatusAcceptsWireEnumAndRejectsUnknownValues(t *testing.T) {
	service, root := runnerWorkspace(t, false)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	started := manualRun(t, manager, root, "status-filter")

	page, rpcErr := manager.ListRuns(ListParams{Workspace: root, Status: "pendingManual"})
	if rpcErr != nil {
		t.Fatalf("ListRuns: %+v", rpcErr)
	}
	found := false
	for _, run := range page.Runs {
		if run.RunID == started.RunID {
			found = true
		}
	}
	if !found {
		t.Fatalf("filtering by the documented wire status %q found nothing: %#v", "pendingManual", page.Runs)
	}

	if _, rpcErr := manager.ListRuns(ListParams{Workspace: root, Status: "not-a-real-status"}); rpcErr == nil || rpcErr.Code != proto.CodeInvalidParams {
		t.Fatalf("unknown status error = %+v, want -32602", rpcErr)
	}
}

func TestListRunsRejectsCursorReferencingAnUnregisteredRoot(t *testing.T) {
	base := t.TempDir()
	registry := workspace.NewRegistry(filepath.Join(base, "workspaces.json"))
	service := workspace.NewService(registry, workspace.Deps{Host: manifest.OSHost{}})
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	forgedRoot := filepath.Join(base, "not-a-registered-workspace")
	cursor := forgeCrossCursor(t, map[string]string{forgedRoot: ""})

	if _, rpcErr := manager.ListRuns(ListParams{Cursor: cursor}); rpcErr == nil {
		t.Fatal("ListRuns with a cursor naming an unregistered root should have errored")
	}
	if _, err := os.Stat(filepath.Join(forgedRoot, ".doppels")); !os.IsNotExist(err) {
		t.Fatalf(".doppels was created outside the registry at %s (stat err = %v)", forgedRoot, err)
	}
}

// TestListRunsRejectsCursorForARemovedWorkspace covers the legitimate
// version of the same bug: a client held a valid cursor, the workspace was
// then removed from the registry, and the client resumes pagination. The
// cursor is now stale — reject it rather than silently creating (or
// resurrecting) filesystem state for a root the registry no longer knows
// about.
func TestListRunsRejectsCursorForARemovedWorkspace(t *testing.T) {
	service, rootA, rootB := twoWorkspaceService(t)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	startAndFinish(t, manager, rootA, "removed-a1")
	startAndFinish(t, manager, rootB, "removed-b1")

	cursor := forgeCrossCursor(t, map[string]string{rootA: "", rootB: ""})
	if _, err := service.RemoveWorkspace(rootB); err != nil {
		t.Fatal(err)
	}

	if _, rpcErr := manager.ListRuns(ListParams{Cursor: cursor}); rpcErr == nil {
		t.Fatal("ListRuns with a cursor for a removed workspace should have errored")
	}
}

func forgeCrossCursor(t *testing.T, roots map[string]string) string {
	t.Helper()
	data, err := json.Marshal(crossCursor{Version: 1, Roots: roots})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// twoWorkspaceService registers two independent, runnable workspaces (each
// with a "greet" Capability+Recipe) under one registry, for tests that need
// genuine cross-workspace aggregation.
func twoWorkspaceService(t *testing.T) (service *workspace.Service, rootA, rootB string) {
	t.Helper()
	base := t.TempDir()
	registry := workspace.NewRegistry(filepath.Join(base, "workspaces.json"))
	service = workspace.NewService(registry, workspace.Deps{Host: manifest.OSHost{}})
	rootA = filepath.Join(base, "a")
	rootB = filepath.Join(base, "b")
	for _, root := range []string{rootA, rootB} {
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
        export VALUE="ok:$DOPPEL_INPUT_COUNT"
    produces:
      value: {env: VALUE}
returns:
  value: "{{ steps.run.value }}"
`)
		if _, _, err := service.AddWorkspace(root); err != nil {
			t.Fatal(err)
		}
	}
	return service, rootA, rootB
}

func TestListRunsAcrossWorkspacesMergesByCreatedAtDesc(t *testing.T) {
	service, rootA, rootB := twoWorkspaceService(t)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	a1 := startAndFinish(t, manager, rootA, "cross-a1")
	b1 := startAndFinish(t, manager, rootB, "cross-b1")
	a2 := startAndFinish(t, manager, rootA, "cross-a2")

	all := map[string]bool{}
	cursor := ""
	for {
		page, rpcErr := manager.ListRuns(ListParams{Limit: 1, Cursor: cursor})
		if rpcErr != nil {
			t.Fatalf("ListRuns: %+v", rpcErr)
		}
		if len(page.Runs) != 1 {
			t.Fatalf("page = %#v, want exactly 1 run per page", page)
		}
		if all[page.Runs[0].RunID] {
			t.Fatalf("run %s returned twice", page.Runs[0].RunID)
		}
		all[page.Runs[0].RunID] = true
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if len(all) > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	for _, id := range []string{a1.RunID, b1.RunID, a2.RunID} {
		if !all[id] {
			t.Fatalf("run %s missing from aggregated pages: %#v", id, all)
		}
	}
}
