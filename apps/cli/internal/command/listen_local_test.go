package command

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
)

func TestListenLocalIndexLoadsRootCapabilities(t *testing.T) {
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	capDir := filepath.Join(root, ".doppels", "capabilities")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "sync-invoices.yaml"), []byte(`apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: sync-invoices, version: 1.1.0}
inputs: {}
outputs: {ok: {type: string}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	recipeDir := filepath.Join(root, ".doppels", "recipes")
	if err := os.MkdirAll(recipeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recipeDir, "sync-invoices.yaml"), []byte(`apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: sync-invoices, version: 1.1.0}
provides: [sync-invoices]
runtime: shell
requires:
  commands: [sh]
defaults: {approval: never}
steps:
  - id: run
    name: Mock
    run:
      shell: sh
      script: |
        export OK=ok
    produces:
      ok: {env: OK}
returns:
  ok: "{{ steps.run.ok }}"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	app, _, stderr := testApp(root)
	index, code := app.listenLocalIndex()
	if code != ExitSuccess {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(index.Entries) != 1 {
		t.Fatalf("entries=%d want 1", len(index.Entries))
	}
	names := listLocalCapabilityNames(index.Entries[0].Catalog)
	if len(names) != 1 || names[0] != "sync-invoices@1.1.0" {
		t.Fatalf("caps=%v", names)
	}
	trees := index.localTrees()
	if len(trees) != 1 || len(trees[0].Capabilities) != 1 {
		t.Fatalf("trees=%#v", trees)
	}
	got := trees[0].Capabilities[0]
	if got.Path != ".doppels/capabilities/sync-invoices.yaml" {
		t.Fatalf("capability path = %q", got.Path)
	}
	if len(got.Recipes) != 1 || got.Recipes[0].Path != ".doppels/recipes/sync-invoices.yaml" {
		t.Fatalf("recipe = %#v", got.Recipes)
	}
	if !got.Recipes[0].Checked || got.Recipes[0].Ready {
		t.Fatalf("unavailable host should fail requires: %#v", got.Recipes[0])
	}
	if strings.Join(got.Recipes[0].Missing, " ") == "" || !strings.Contains(strings.Join(got.Recipes[0].Missing, " "), "sh") {
		t.Fatalf("missing = %#v", got.Recipes[0].Missing)
	}
}

func TestListenLocalIndexStaysOnCwdProject(t *testing.T) {
	workspace := t.TempDir()
	here := filepath.Join(workspace, "here")
	sibling := filepath.Join(workspace, "sibling")
	stale := t.TempDir()
	for _, root := range []string{here, sibling, stale} {
		if _, err := project.Init(root); err != nil {
			t.Fatal(err)
		}
	}
	app, _, stderr := testApp(here)
	store := configstore.New(filepath.Join(here, ".test-config"))
	if err := store.SetContext(configstore.LocalContext()); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBinding(configstore.LocalOrganization, "sibling", sibling); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBinding(configstore.LocalOrganization, "stale", stale); err != nil {
		t.Fatal(err)
	}

	index, code := app.listenLocalIndex()
	if code != ExitSuccess {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(index.Entries) != 1 {
		t.Fatalf("entries=%d want 1: %#v", len(index.Entries), rootsOf(index))
	}
	want, _ := filepath.Abs(here)
	got, _ := filepath.Abs(index.Entries[0].Root)
	if got != want {
		t.Fatalf("root=%q want %q", got, want)
	}
}

func TestListenLocalIndexLoadsImmediateChildSpaces(t *testing.T) {
	workspace := t.TempDir()
	a := filepath.Join(workspace, "a")
	b := filepath.Join(workspace, "b")
	if _, err := project.Init(a); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Init(b); err != nil {
		t.Fatal(err)
	}
	app, _, stderr := testApp(workspace)
	index, code := app.listenLocalIndex()
	if code != ExitSuccess {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(index.Entries) != 2 {
		t.Fatalf("entries=%d want 2: %#v", len(index.Entries), rootsOf(index))
	}
}

func TestListenLocalIndexIgnoresParentProject(t *testing.T) {
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	app, _, stderr := testApp(nested)
	_, code := app.listenLocalIndex()
	if code != ExitContract {
		t.Fatalf("code=%d want %d stderr=%s", code, ExitContract, stderr.String())
	}
}

func rootsOf(index listenLocalIndex) []string {
	roots := make([]string, 0, len(index.Entries))
	for _, entry := range index.Entries {
		roots = append(roots, entry.Root)
	}
	return roots
}

func TestListenLocalIndexResolveBySpace(t *testing.T) {
	t.Parallel()
	engCatalog := manifest.NewCatalog("eng", []manifest.Loaded{
		{Document: &manifest.Capability{Metadata: manifest.Metadata{Name: "create-preview", Version: "1.0.0"}}},
	})
	finCatalog := manifest.NewCatalog("fin", []manifest.Loaded{
		{Document: &manifest.Capability{Metadata: manifest.Metadata{Name: "create-preview", Version: "1.0.0"}}},
		{Document: &manifest.Capability{Metadata: manifest.Metadata{Name: "sync-invoices", Version: "1.0.0"}}},
	})
	index := listenLocalIndex{Entries: []listenProjectEntry{
		{Root: "/ws/engineering", Space: "engineering", Label: "engineering", Catalog: engCatalog},
		{Root: "/ws/finance", Space: "finance", Label: "finance", Catalog: finCatalog},
	}}
	root, catalog, err := index.resolve("create-preview", "engineering")
	if err != nil || root != "/ws/engineering" || catalog != engCatalog {
		t.Fatalf("resolve = %q %#v %v", root, catalog, err)
	}
	root, catalog, err = index.resolve("create-preview", "finance")
	if err != nil || root != "/ws/finance" || catalog != finCatalog {
		t.Fatalf("space prefer = %q %#v %v", root, catalog, err)
	}
	_, _, err = index.resolve("missing", "")
	if err == nil {
		t.Fatal("expected missing capability error")
	}
}

func TestInspectGitWorktreePrimary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q", "-b", "main").Run(); err != nil {
		if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
			t.Skipf("git init: %v", err)
		}
	}
	branch, worktree := inspectGitWorktree(dir)
	if branch != "main" && branch != "master" {
		t.Fatalf("branch = %q", branch)
	}
	if worktree != "primary" {
		t.Fatalf("worktree = %q", worktree)
	}
}

func TestListenSpaceHint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "doppels.engineering.yaml")
	if err := os.WriteFile(path, []byte("kind: Space\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := listenSpaceHint(dir); got != "engineering" {
		t.Fatalf("hint = %q", got)
	}
}

func TestListenLocalIndexLoadsChildWithOnlySpaceStub(t *testing.T) {
	workspace := t.TempDir()
	child := filepath.Join(workspace, "engineering")
	if err := os.MkdirAll(filepath.Join(child, ".doppels", "capabilities"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "doppels.engineering.yaml"), []byte(`apiVersion: doppels.so/v1alpha1
kind: Space
metadata:
  name: engineering
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, ".doppels", "capabilities", "greet.yaml"), []byte(runCapabilityFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	app, _, stderr := testApp(workspace)
	index, code := app.listenLocalIndex()
	if code != ExitSuccess {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(index.Entries) != 1 {
		t.Fatalf("entries=%d want 1: %#v", len(index.Entries), rootsOf(index))
	}
	names := listLocalCapabilityNames(index.Entries[0].Catalog)
	if len(names) != 1 || names[0] != "greet@1.0.0" {
		t.Fatalf("caps=%v", names)
	}
}

func TestWriteNodeMissingSpaceExplainsWorkspace(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeNodeMissingSpace(&buf, "/Users/turbo/projects/opendoppel")
	out := buf.String()
	for _, want := range []string{
		"No Space",
		"/Users/turbo/projects/opendoppel",
		"A Space is a folder with .doppels/ (capabilities/, recipes/, runtime)",
		"discovery:",
		"doppels.",
		"workspace",
		"immediate",
		"doppels init",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
