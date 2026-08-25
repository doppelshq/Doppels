package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doppels.so/cli/internal/manifest"
)

type unavailableHost struct{}

func (unavailableHost) LookupCommand(string) (string, error)  { return "", errors.New("not found") }
func (unavailableHost) LookupEnv(string) (string, bool)       { return "", false }
func (unavailableHost) Stat(string) (fs.FileInfo, error)      { return nil, os.ErrNotExist }
func (unavailableHost) CommandVersion(string) (string, error) { return "", errors.New("not found") }

func testApp(root string) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &App{
		Stdout:    stdout,
		Stderr:    stderr,
		Getwd:     func() (string, error) { return root, nil },
		ConfigDir: func() (string, error) { return filepath.Join(root, ".test-config"), nil },
		Host:      unavailableHost{},
	}, stdout, stderr
}

// writeManifest writes a YAML file under .doppels/<directory>/<name> and
// ensures the .doppels marker directory exists so FindRoot succeeds.
func writeManifest(t *testing.T, root, directory, name, value string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".doppels"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := directory
	if directory == "capabilities" || directory == "recipes" {
		base = filepath.Join(".doppels", directory)
	}
	path := filepath.Join(root, base, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNoArgsPrintsUsage(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	if code := app.Run(nil); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Doppels",
		"Space",
		"Run",
		"Tooling",
		"doppels init",
		"doppels run",
		"doppels runs [list]",
		"doppels capabilities|caps",
		"doppels update",
		"doppels runs [list]",
		"--json",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage missing %q\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should stay clean, got %s", stderr.String())
	}
	if strings.Contains(out, "doppels plan|") || strings.Contains(out, "doppels plan ") {
		t.Fatalf("usage still advertises plan:\n%s", out)
	}
}

func TestPlanIsUnknownCommand(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"plan"}); code != ExitContract {
		t.Fatalf("plan exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `unknown command "plan"`) {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestListenIsUnknownCommand(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_EXPERIMENTAL=1"}
	if code := app.Run([]string{"listen"}); code != ExitContract {
		t.Fatalf("listen exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `unknown command "listen"`) {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestNodeWithoutSubcommandPrintsUsage(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"node"}); code != ExitContract {
		t.Fatalf("node exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "doppels node up") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestNodeUpRequiresLogin(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"node", "up"}); code == ExitSuccess {
		t.Fatalf("node up succeeded without login: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "login or DOPPELS_API_TOKEN is required for node up") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestExperimentalHelpShowsNodeUpNotListen(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_EXPERIMENTAL=1"}
	if code := app.Run([]string{"help"}); code != ExitSuccess {
		t.Fatalf("help exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "doppels node up") {
		t.Fatalf("help missing node up:\n%s", out)
	}
	if strings.Contains(out, "doppels listen") {
		t.Fatalf("help still advertises listen:\n%s", out)
	}
	if !strings.Contains(out, "· Node") {
		t.Fatalf("help missing Node section:\n%s", out)
	}
}

func TestHelpStillPrintsUsage(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"help"}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "doppels validate") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestInitValidateAndDescribe(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"init", "platform"}); code != ExitSuccess {
		t.Fatalf("init platform exit = %d, stderr = %s", code, stderr.String())
	}
	doppelsDir := filepath.Join(root, ".doppels")
	for _, sub := range []string{"capabilities", "recipes"} {
		if _, err := os.Stat(filepath.Join(doppelsDir, sub)); err != nil {
			t.Fatalf("missing .doppels/%s: %v", sub, err)
		}
	}
	if _, err := os.Stat(filepath.Join(doppelsDir, "platform.space.yaml")); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"init"}); code != ExitSuccess {
		t.Fatalf("init exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "local/private") {
		t.Fatalf("init stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	writeManifest(t, root, "capabilities", "release.yaml", capabilityFixture)
	if code := app.Run([]string{"validate"}); code != ExitSuccess {
		t.Fatalf("validate exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Validated") || !strings.Contains(stdout.String(), "Capability") || !strings.Contains(stdout.String(), "capability/release-build@") {
		t.Fatalf("validate stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"describe", "--json", "capability/release-build"}); code != ExitSuccess {
		t.Fatalf("describe exit = %d, stderr = %s", code, stderr.String())
	}
	var response struct {
		Kind   string `json:"kind"`
		Source struct {
			SHA256 string `json:"sha256"`
		} `json:"source"`
		Recipes []any `json:"recipes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	if response.Kind != "CapabilityDescription" || len(response.Source.SHA256) != 64 || len(response.Recipes) != 0 {
		t.Fatalf("unexpected describe response: %#v", response)
	}
}

func TestValidateContractAndHostExitCodes(t *testing.T) {
	t.Run("contract", func(t *testing.T) {
		root := t.TempDir()
		app, stdout, stderr := testApp(root)
		writeManifest(t, root, "capabilities", "invalid.yaml", strings.Replace(capabilityFixture, "outputs:", "unexpected: true\noutputs:", 1))
		code := app.Run([]string{"validate", "--json"})
		if code != ExitContract {
			t.Fatalf("exit = %d, want %d", code, ExitContract)
		}
		if stderr.Len() != 0 {
			t.Fatalf("JSON mode wrote stderr: %s", stderr.String())
		}
		if !strings.Contains(stdout.String(), `"status": "invalid"`) {
			t.Fatalf("stdout = %s", stdout.String())
		}
	})

	t.Run("host", func(t *testing.T) {
		root := t.TempDir()
		app, _, stderr := testApp(root)
		writeManifest(t, root, "capabilities", "release.yaml", capabilityFixture)
		writeManifest(t, root, "recipes", "release.yaml", recipeFixture)
		code := app.Run([]string{"validate"})
		if code != ExitOperational {
			t.Fatalf("exit = %d, want %d; %s", code, ExitOperational, stderr.String())
		}
		out := stderr.String()
		if !strings.Contains(out, "Recipe not ready") || !strings.Contains(out, "missing-tool") {
			t.Fatalf("stderr missing host presentation:\n%s", out)
		}
		if strings.Contains(out, "host.command-missing") || strings.Contains(out, "requires.commands[0]") {
			t.Fatalf("stderr still dumps validator noise:\n%s", out)
		}
	})
}

func TestDescribeShowsAmbiguousRecipesWithoutSelectingOne(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	writeManifest(t, root, "capabilities", "release.yaml", capabilityFixture)
	writeManifest(t, root, "recipes", "first.yaml", manualRecipeFixture)
	writeManifest(t, root, "recipes", "second.yaml", strings.Replace(manualRecipeFixture, "name: release-human", "name: release-review", 1))
	if code := app.Run([]string{"describe", "capability/release-build"}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "explicit --recipe selection will be required") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestValidateExplicitFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "capability.yaml")
	if err := os.WriteFile(path, []byte(capabilityFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	app, stdout, stderr := testApp(root)
	app.Host = manifest.OSHost{}
	if code := app.Run([]string{"validate", "-f", path}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Validated") || !strings.Contains(stdout.String(), "capability/release-build@") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

const capabilityFixture = `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata:
  name: release-build
  version: 1.0.0
  summary: Build a release.
inputs: {}
outputs:
  archive:
    type: artifact
`

const recipeFixture = `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: release-shell, version: 1.0.0}
provides: [release-build]
runtime: shell
requires:
  commands: [missing-tool]
defaults: {approval: never}
steps:
  - id: build
    name: Build
    run: {shell: sh, script: touch archive.tgz}
    produces:
      archive: {file: archive.tgz}
returns:
  archive: "{{ steps.build.archive }}"
`

const manualRecipeFixture = `apiVersion: doppels.so/v1alpha1
kind: Recipe
metadata: {name: release-human, version: 1.0.0}
provides: [release-build]
runtime: manual
procedure: {readme: ./runbook.md}
evidence:
  notes: {type: string}
`
