package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/projectlock"
)

func TestRunResourceFirstKeepsJSONStdoutMachineReadable(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--json"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "step-noise") {
		t.Fatalf("runtime output leaked to JSON stdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "step-noise") {
		t.Fatalf("runtime output missing from stderr: %s", stderr.String())
	}
	var result struct {
		Kind    string         `json:"kind"`
		Status  string         `json:"status"`
		Returns map[string]any `json:"returns"`
		Run     struct {
			NodeID string `json:"nodeId"`
		} `json:"run"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if result.Kind != "LocalRunResult" || result.Status != "succeeded" || result.Returns["message"] != "Hello Ada" || result.Run.NodeID != "test-node" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunCapabilityWithoutRecipeCollectsManualOutputs(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "manual.yaml", `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: manual-answer, version: 1.0.0}
inputs: {}
outputs:
  answer: {type: integer}
  receipt: {type: artifact, mediaType: text/plain}
`)
	if err := os.WriteFile(filepath.Join(root, "answer.txt"), []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_IDENTITY=owner"}
	app.Hostname = func() (string, error) { return "node", nil }

	code := app.Run([]string{"run", "capability/manual-answer", "--output", "answer=42", "--output", "receipt=answer.txt", "--json"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var result struct {
		Status  string         `json:"status"`
		Returns map[string]any `json:"returns"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "succeeded" || result.Returns["answer"] != float64(42) {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunRequiresExplicitRecipeWhenSelectionIsAmbiguous(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "one.yaml", runRecipeFixture)
	writeManifest(t, root, "recipes", "two.yaml", strings.Replace(runRecipeFixture, "name: greet-shell", "name: greet-other", 1))
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada"}); code != ExitContract {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "multiple compatible Recipes") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestRunPrintsLiveTimeline(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	progress := stderr.String()
	for _, want := range []string{"Cap", "greet", "Recipe", "validated", "Greet"} {
		if !strings.Contains(progress, want) {
			t.Fatalf("timeline missing %q in stderr:\n%s", want, progress)
		}
	}
	summary := stdout.String()
	if !strings.Contains(summary, "Succeeded") || !strings.Contains(summary, "Returns") {
		t.Fatalf("summary stdout = %s", summary)
	}
	if !strings.Contains(summary, "ms") && !strings.Contains(summary, "s") {
		t.Fatalf("summary missing elapsed timing: %s", summary)
	}
	if strings.Contains(summary, "Run       ") || strings.Contains(summary, "Status    succeeded") {
		t.Fatalf("human summary should use compact status block: %s", summary)
	}
}

func TestRunAutoApprovesRequiredSteps(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", strings.Replace(runRecipeFixture, "approval: never", "approval: required", 1))
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--json"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "Approve") {
		t.Fatalf("local run should not prompt for Step approval: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status": "succeeded"`) && !strings.Contains(stdout.String(), `"status":"succeeded"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestRunPromptsForCapabilityAndRequiredInputs(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "capabilities", "other.yaml", `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: other-task, version: 1.0.0, displayName: Other}
inputs: {}
outputs: {ok: {type: string}}
`)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Stdin = strings.NewReader("1\nAda\n")
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }

	code := app.Run([]string{"run"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Run a Capability") {
		t.Fatalf("expected capability picker, stderr = %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Input") || !strings.Contains(stderr.String(), "name") {
		t.Fatalf("expected input prompt, stderr = %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Succeeded") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestRunPromptsForMissingInputsWhenCapabilityGiven(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Stdin = strings.NewReader("Ada\n")
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }

	code := app.Run([]string{"run", "capability/greet"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "name") {
		t.Fatalf("expected input prompt, stderr = %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Succeeded") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestRunRequiresCapabilityWithoutPromptableStdin(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"run"}); code != ExitContract {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "run requires capability/") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestRunStrictBlocksStaleLockPin(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	writeStaleCapabilityLock(t, root)

	app, _, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--yes", "--strict", "--json"})
	if code != ExitContract {
		t.Fatalf("strict stale exit = %d, want %d, stderr = %s", code, ExitContract, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "stale") || !strings.Contains(out, "version bump") {
		t.Fatalf("strict stale stderr = %s", out)
	}
}

func TestRunWithoutStrictWarnsOnStaleLockPin(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	writeStaleCapabilityLock(t, root)

	app, stdout, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }
	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--yes", "--json"})
	if code != ExitSuccess {
		t.Fatalf("stale without --strict exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "stale") {
		t.Fatalf("expected stale warning, stderr = %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status": "succeeded"`) && !strings.Contains(stdout.String(), `"status":"succeeded"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func writeStaleCapabilityLock(t *testing.T, root string) {
	t.Helper()
	if err := projectlock.Write(root, projectlock.New([]projectlock.Entry{{
		Kind:            "Capability",
		SourceAuthority: "manifest",
		Revision: execution.DefinitionReference{
			Name: "greet", Version: "1.0.0", ManifestSHA256: strings.Repeat("ab", 32),
			Schema: execution.SchemaReference{ID: manifest.CapabilitySchemaID, SHA256: manifest.CapabilitySchemaSHA256},
		},
	}})); err != nil {
		t.Fatal(err)
	}
}

func TestBuildDetachedRunArgsIncludesResolvedInputs(t *testing.T) {
	got := buildDetachedRunArgs("capability/greet@1.0.0", "greet-shell@1.0.0", map[string]string{"name": "Ada"}, false)
	want := []string{"--recipe", "greet-shell@1.0.0", "--input", "name=Ada", "capability/greet@1.0.0"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := map[time.Duration]string{
		50 * time.Millisecond:                 "50ms",
		999 * time.Millisecond:                "999ms",
		1500 * time.Millisecond:               "1s",
		10*time.Second + 100*time.Millisecond: "10s",
		65 * time.Second:                      "1m05s",
	}
	for d, want := range tests {
		if got := formatDuration(d); got != want {
			t.Fatalf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if got := formatDurationLive(1500 * time.Millisecond); got != "1s" {
		t.Fatalf("formatDurationLive(1.5s) = %q", got)
	}
	if got := formatDurationLive(400 * time.Millisecond); got != "0s" {
		t.Fatalf("formatDurationLive(400ms) = %q", got)
	}
}

const runCapabilityFixture = `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: greet, version: 1.0.0}
inputs:
  name: {type: string, required: true}
outputs:
  message: {type: string}
`

const runRecipeFixture = `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: greet-shell, version: 1.0.0}
provides: [greet]
runtime: shell
requires:
  commands: [sh]
defaults: {approval: never}
steps:
  - id: greet
    name: Greet
    env:
      NAME: "{{ inputs.name }}"
    run:
      shell: sh
      script: |
        printf 'step-noise\n'
        export MESSAGE="Hello $NAME"
    produces:
      message: {env: MESSAGE}
returns:
  message: "{{ steps.greet.message }}"
`
