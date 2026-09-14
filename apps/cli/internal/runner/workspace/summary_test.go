package workspace

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/projectlock"
	"doppels.so/cli/internal/runner/proto"
)

const shellCapabilityFixture = `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: sync-invoices, version: 1.0.0}
inputs: {}
outputs: {ok: {type: string}}
`

const shellRecipeFixture = `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: sync-invoices, version: 1.0.0}
provides: [sync-invoices]
runtime: shell
requires:
  commands: [sh, missing-tool]
defaults: {approval: never}
steps:
  - id: run
    name: Mock
    run: {shell: sh, script: "export OK=ok"}
    produces:
      ok: {env: OK}
returns:
  ok: "{{ steps.run.ok }}"
`

const manualCapabilityFixture = `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: answer-question, version: 1.0.0}
inputs: {}
outputs: {ok: {type: string}}
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newSpace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBuildWorkspaceSummaryHealthMissingRoot(t *testing.T) {
	summary := BuildWorkspaceSummary(filepath.Join(t.TempDir(), "gone"), Deps{Host: manifest.OSHost{}})
	if summary.Health != "missingRoot" {
		t.Fatalf("Health = %q, want missingRoot", summary.Health)
	}
}

func TestBuildWorkspaceSummaryHealthOK(t *testing.T) {
	root := newSpace(t)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "sync-invoices.yaml"), shellCapabilityFixture)
	writeFile(t, filepath.Join(root, ".doppels", "recipes", "sync-invoices.yaml"), shellRecipeFixture)

	summary := BuildWorkspaceSummary(root, Deps{Host: manifest.OSHost{}})
	if summary.Health != "ok" {
		t.Fatalf("Health = %q, want ok", summary.Health)
	}
	if summary.Capabilities != 1 {
		t.Fatalf("Capabilities = %d, want 1", summary.Capabilities)
	}
	if summary.Recipes != 1 {
		t.Fatalf("Recipes = %d, want 1", summary.Recipes)
	}
	if summary.Root != root {
		t.Fatalf("Root = %q, want %q", summary.Root, root)
	}
}

func TestBuildWorkspaceSummaryUsesSpaceManifestName(t *testing.T) {
	root := newSpace(t)
	if _, _, err := project.WriteSpaceManifest(root, "engineering"); err != nil {
		t.Fatal(err)
	}

	summary := BuildWorkspaceSummary(root, Deps{Host: manifest.OSHost{}})
	if summary.Space != "engineering" {
		t.Fatalf("Space = %q, want metadata.name from .doppels Space manifest", summary.Space)
	}
}

func TestBuildWorkspaceSummaryUsesDeclaredDiscovery(t *testing.T) {
	root := newSpace(t)
	writeFile(t, filepath.Join(root, ".doppels", "engineering.space.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Space
metadata: {name: engineering}
discovery:
  capabilities: [catalog/capabilities]
  recipes: [catalog/recipes]
`)
	writeFile(t, filepath.Join(root, "catalog", "capabilities", "answer.yaml"), manualCapabilityFixture)

	summary := BuildWorkspaceSummary(root, Deps{Host: manifest.OSHost{}})
	if summary.Health != "ok" || summary.Capabilities != 1 || summary.Space != "engineering" {
		t.Fatalf("summary = %+v, want custom discovery and Space metadata", summary)
	}
}

func TestBuildWorkspaceSummaryHealthInvalidManifests(t *testing.T) {
	root := newSpace(t)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "broken.yaml"), "kind: Capability\nmetadata: {}\n")

	summary := BuildWorkspaceSummary(root, Deps{Host: manifest.OSHost{}})
	if summary.Health != "invalidManifests" {
		t.Fatalf("Health = %q, want invalidManifests", summary.Health)
	}
}

func TestBuildWorkspaceSummaryGitBranch(t *testing.T) {
	root := newSpace(t)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.co", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.co")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v: %s", err, out)
		}
	}
	run("init", "-q", "-b", "feature/runner")
	run("commit", "--allow-empty", "-q", "-m", "init")

	summary := BuildWorkspaceSummary(root, Deps{Host: manifest.OSHost{}})
	if summary.GitBranch == nil || *summary.GitBranch != "feature/runner" {
		t.Fatalf("GitBranch = %v, want feature/runner", summary.GitBranch)
	}
}

func TestBuildCapabilitySummariesRuntimeNoneWithoutRecipe(t *testing.T) {
	root := newSpace(t)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "answer.yaml"), manualCapabilityFixture)

	summaries := capabilitiesForTest(t, root)
	if len(summaries) != 1 {
		t.Fatalf("len(summaries) = %d, want 1", len(summaries))
	}
	if summaries[0].Runtime != "none" {
		t.Fatalf("Runtime = %q, want none", summaries[0].Runtime)
	}
	if summaries[0].Recipe != nil {
		t.Fatalf("Recipe = %+v, want nil", summaries[0].Recipe)
	}
	if len(summaries[0].Readiness) != 0 {
		t.Fatalf("Readiness = %+v, want empty", summaries[0].Readiness)
	}
}

func TestBuildCapabilitySummariesShellReadinessReportsPerCommand(t *testing.T) {
	root := newSpace(t)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "sync-invoices.yaml"), shellCapabilityFixture)
	writeFile(t, filepath.Join(root, ".doppels", "recipes", "sync-invoices.yaml"), shellRecipeFixture)

	summaries := capabilitiesForTest(t, root)
	if len(summaries) != 1 {
		t.Fatalf("len(summaries) = %d, want 1", len(summaries))
	}
	summary := summaries[0]
	if summary.Runtime != "shell" {
		t.Fatalf("Runtime = %q, want shell", summary.Runtime)
	}
	if summary.Recipe == nil || summary.Recipe.Name != "sync-invoices" {
		t.Fatalf("Recipe = %+v", summary.Recipe)
	}
	if len(summary.ManifestSHA256) != 64 || len(summary.Recipe.ManifestSHA256) != 64 {
		t.Fatalf("digests = capability %q recipe %q, want SHA-256 hex", summary.ManifestSHA256, summary.Recipe.ManifestSHA256)
	}
	if len(summary.Readiness) != 2 {
		t.Fatalf("Readiness = %+v, want 2 checks", summary.Readiness)
	}
	byCommand := map[string]bool{}
	for _, check := range summary.Readiness {
		byCommand[check.Command] = check.OK
	}
	if !byCommand["sh"] {
		t.Fatalf("readiness for sh = %v, want ok", byCommand["sh"])
	}
	if byCommand["missing-tool"] {
		t.Fatalf("readiness for missing-tool = %v, want not ok", byCommand["missing-tool"])
	}
}

func TestBuildCapabilitySummariesManualRuntime(t *testing.T) {
	root := newSpace(t)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "answer.yaml"), manualCapabilityFixture)
	writeFile(t, filepath.Join(root, ".doppels", "recipes", "answer.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: answer-human, version: 1.0.0}
provides: [answer-question]
runtime: manual
procedure: {readme: ./runbook.md}
evidence: {notes: {type: string}}
`)

	summaries := capabilitiesForTest(t, root)
	if len(summaries) != 1 || summaries[0].Runtime != "manual" || summaries[0].Recipe == nil || summaries[0].Recipe.Name != "answer-human" || len(summaries[0].Readiness) != 0 {
		t.Fatalf("summaries = %+v, want manual recipe without command readiness", summaries)
	}
}

func TestBuildCapabilitySummariesDuplicateVersionsBothListed(t *testing.T) {
	root := newSpace(t)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "answer-v1.yaml"), manualCapabilityFixture)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "answer-v2.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: answer-question, version: 2.0.0}
inputs: {}
outputs: {ok: {type: string}}
`)

	summaries := capabilitiesForTest(t, root)
	if len(summaries) != 2 {
		t.Fatalf("len(summaries) = %d, want 2", len(summaries))
	}
	if summaries[0].Version != "1.0.0" || summaries[1].Version != "2.0.0" {
		t.Fatalf("versions = %q, %q; want ascending 1.0.0, 2.0.0", summaries[0].Version, summaries[1].Version)
	}
}

func TestBuildCapabilitySummariesDuplicateRevisionOrderingIsDeterministic(t *testing.T) {
	root := newSpace(t)
	first := `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: answer-question, version: 1.0.0, summary: first}
inputs: {}
outputs: {ok: {type: string}}
`
	second := `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: answer-question, version: 1.0.0, summary: second}
inputs: {}
outputs: {ok: {type: string}}
`
	firstDigest := sha256.Sum256([]byte(first))
	secondDigest := sha256.Sum256([]byte(second))
	if string(firstDigest[:]) < string(secondDigest[:]) {
		first, second = second, first
		firstDigest, secondDigest = secondDigest, firstDigest
	}
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "a.yaml"), first)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "b.yaml"), second)

	summaries := capabilitiesForTest(t, root)
	if len(summaries) != 2 || summaries[0].ManifestSHA256 != fmt.Sprintf("%x", secondDigest) || summaries[1].ManifestSHA256 != fmt.Sprintf("%x", firstDigest) {
		t.Fatalf("summaries = %+v, want digest tie-break ordering", summaries)
	}
}

func TestBuildCapabilitySummariesPinStates(t *testing.T) {
	root := newSpace(t)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "answer.yaml"), manualCapabilityFixture)

	unpinned := capabilitiesForTest(t, root)
	if len(unpinned) != 1 || unpinned[0].Pin != "unpinned" {
		t.Fatalf("Pin = %+v, want unpinned", unpinned)
	}

	loadedManifest, err := manifest.Load(filepath.Join(root, ".doppels", "capabilities", "answer.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lock := projectlock.New([]projectlock.Entry{{
		Kind:            "Capability",
		SourceAuthority: "manifest",
		Revision: execution.DefinitionReference{
			Name: "answer-question", Version: "1.0.0", ManifestSHA256: loadedManifest.SHA256,
			Schema: execution.SchemaReference{ID: manifest.CapabilitySchemaID, SHA256: manifest.CapabilitySchemaSHA256},
		},
	}})
	if err := projectlock.Write(root, lock); err != nil {
		t.Fatal(err)
	}
	pinned := capabilitiesForTest(t, root)
	if len(pinned) != 1 || pinned[0].Pin != "pinned" {
		t.Fatalf("Pin = %+v, want pinned", pinned)
	}

	// Change the manifest without bumping version: digest no longer matches
	// the lock entry, so the pin state goes stale.
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "answer.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: answer-question, version: 1.0.0}
inputs: {}
outputs: {ok: {type: string}, extra: {type: string}}
`)
	stale := capabilitiesForTest(t, root)
	if len(stale) != 1 || stale[0].Pin != "stale" {
		t.Fatalf("Pin = %+v, want stale", stale)
	}
}

func capabilitiesForTest(t *testing.T, root string) []proto.CapabilitySummary {
	t.Helper()
	state := loadWorkspaceState(root, Deps{Host: manifest.OSHost{}})
	if state.Catalog == nil {
		t.Fatalf("expected a catalog, health=%s", state.Health)
	}
	return buildCapabilitySummaries(root, state.Catalog, state.Lock, Deps{Host: manifest.OSHost{}})
}
