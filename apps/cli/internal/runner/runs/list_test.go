package runs

import (
	"context"
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
	if len(page.Runs) != 1 || page.Runs[0].RunID != second.RunID || page.NextCursor == "" {
		t.Fatalf("first page = %#v", page)
	}
	next, rpcErr := manager.ListRuns(ListParams{Workspace: root, Limit: 1, Cursor: page.NextCursor})
	if rpcErr != nil {
		t.Fatalf("ListRuns page 2: %+v", rpcErr)
	}
	if len(next.Runs) != 1 || next.Runs[0].RunID != first.RunID || next.NextCursor != "" {
		t.Fatalf("second page = %#v", next)
	}

	if _, rpcErr := manager.ListRuns(ListParams{Workspace: "/does/not/exist"}); rpcErr == nil || rpcErr.Code != proto.CodeWorkspaceNotFound {
		t.Fatalf("unknown workspace error = %+v, want -32003", rpcErr)
	}
}

func TestListRunsAcrossWorkspacesMergesByCreatedAtDesc(t *testing.T) {
	base := t.TempDir()
	registry := workspace.NewRegistry(filepath.Join(base, "workspaces.json"))
	service := workspace.NewService(registry, workspace.Deps{Host: manifest.OSHost{}})
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
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
