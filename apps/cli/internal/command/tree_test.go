package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doppels.so/cli/internal/project"
)

func TestTreeListsWorkspaceSpacesCapsRecipes(t *testing.T) {
	workspace := t.TempDir()
	engineering := filepath.Join(workspace, "engineering")
	finance := filepath.Join(workspace, "finance")
	if _, err := project.Init(engineering); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Init(finance); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, engineering, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, engineering, "recipes", "greet.yaml", runRecipeFixture)
	writeManifest(t, finance, "capabilities", "greet.yaml", strings.ReplaceAll(runCapabilityFixture, "greet", "close-month"))
	writeManifest(t, finance, "recipes", "close-month.yaml", strings.ReplaceAll(runRecipeFixture, "greet", "close-month"))

	app, stdout, stderr := testApp(workspace)
	if code := app.Run([]string{"tree"}); code != ExitSuccess {
		t.Fatalf("tree exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Local",
		"engineering",
		"finance",
		"greet@1.0.0",
		"close-month@1.0.0",
		"greet-shell@1.0.0",
		"close-month-shell@1.0.0",
		"├──",
		"└──",
		".doppels/capabilities/greet.yaml",
		".doppels/recipes/greet.yaml",
		"unpinned",
		"1 cap · 1 recipe · 1 blocked",
		"shell",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Status") {
		t.Fatalf("tree should not render node-up Status banner:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should stay clean, got %s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"tree", "--json"}); code != ExitSuccess {
		t.Fatalf("tree json exit = %d, stderr = %s", code, stderr.String())
	}
	payload := stdout.String()
	if !strings.Contains(payload, `"kind": "CatalogTree"`) {
		t.Fatalf("json missing CatalogTree kind: %s", payload)
	}
	var decoded struct {
		Kind   string `json:"kind"`
		Spaces []struct {
			Space  string `json:"space"`
			Counts struct {
				Caps    int `json:"caps"`
				Recipes int `json:"recipes"`
				Blocked int `json:"blocked"`
			} `json:"counts"`
			Capabilities []struct {
				Name    string `json:"name"`
				Mode    string `json:"mode"`
				Pin     string `json:"pin"`
				Recipes []struct {
					Name    string `json:"name"`
					Runtime string `json:"runtime"`
				} `json:"recipes"`
			} `json:"capabilities"`
		} `json:"spaces"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("json: %v\n%s", err, payload)
	}
	if decoded.Kind != "CatalogTree" || len(decoded.Spaces) != 2 {
		t.Fatalf("spaces=%d payload=%s", len(decoded.Spaces), payload)
	}
	found := map[string]bool{}
	for _, space := range decoded.Spaces {
		found[space.Space] = true
		if len(space.Capabilities) != 1 || len(space.Capabilities[0].Recipes) != 1 {
			t.Fatalf("space %q catalog = %#v", space.Space, space)
		}
		if space.Counts.Caps != 1 || space.Counts.Recipes != 1 || space.Counts.Blocked != 1 {
			t.Fatalf("space %q counts = %#v", space.Space, space.Counts)
		}
		if space.Capabilities[0].Mode != "recipe" {
			t.Fatalf("mode = %q", space.Capabilities[0].Mode)
		}
		if space.Capabilities[0].Recipes[0].Runtime != "shell" {
			t.Fatalf("runtime = %#v", space.Capabilities[0].Recipes)
		}
	}
	if !found["engineering"] || !found["finance"] {
		t.Fatalf("spaces = %#v", decoded.Spaces)
	}
}

func TestTreeMarksManualCapability(t *testing.T) {
	root := t.TempDir()
	if _, err := project.Init(root); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, root, "capabilities", "manual-review.yaml", `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: manual-review, version: 1.0.0}
inputs: {}
outputs: {ok: {type: string}}
`)
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"tree"}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "manual-review@1.0.0  manual") {
		t.Fatalf("stdout = %s", out)
	}
	if strings.Contains(out, "blocked") {
		t.Fatalf("manual Capability should not count as blocked Recipe:\n%s", out)
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"tree", "--json"}); code != ExitSuccess {
		t.Fatalf("json exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"mode": "manual"`) {
		t.Fatalf("json = %s", stdout.String())
	}
}

func TestTreeStaysOnCwdSpace(t *testing.T) {
	workspace := t.TempDir()
	here := filepath.Join(workspace, "engineering")
	sibling := filepath.Join(workspace, "finance")
	if _, err := project.Init(here); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Init(sibling); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, here, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, sibling, "capabilities", "close-month.yaml", strings.ReplaceAll(runCapabilityFixture, "greet", "close-month"))

	app, stdout, stderr := testApp(here)
	if code := app.Run([]string{"tree", "--json"}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	payload := stdout.String()
	if !strings.Contains(payload, `"name": "greet"`) {
		t.Fatalf("stdout = %s", payload)
	}
	if strings.Contains(payload, "close-month") {
		t.Fatalf("cwd Space leaked sibling catalog: %s", payload)
	}
}

func TestTreeMissingSpaceSameAsNodeUp(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"tree"}); code != ExitContract {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout should stay clean, got %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "No Space working tree in this directory.") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestTreeHelp(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"tree", "--help"}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: doppels tree [--json]") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should stay clean, got %s", stderr.String())
	}
}
