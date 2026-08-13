package command

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFilterDetachArgs(t *testing.T) {
	got := filterDetachArgs([]string{"capability/greet", "--detach", "--input", "name=Ada", "-d"})
	want := []string{"capability/greet", "--input", "name=Ada"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterDetachArgs = %#v, want %#v", got, want)
	}
}

func TestRunDetachRejectsManual(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "manual.yaml", `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: manual-answer, version: 1.0.0}
inputs: {}
outputs:
  answer: {type: integer}
`)
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"run", "capability/manual-answer", "--detach", "--output", "answer=1"}); code != ExitContract {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--detach requires a shell Recipe") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestRunDetachAllowsApprovalRequiredSteps(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", strings.Replace(runRecipeFixture, "approval: never", "approval: required", 1))
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }
	app.Executable = func() (string, error) { return "/bin/true", nil }
	app.StartCommand = func(cmd *exec.Cmd) error {
		cmd.Process = &os.Process{Pid: 4242}
		return nil
	}
	if code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--detach", "--json"}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var payload struct {
		Kind  string `json:"kind"`
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if payload.Kind != "DetachedRun" || payload.RunID == "" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestRunDetachStartsBackgroundWorker(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }
	app.Executable = func() (string, error) { return "/bin/true", nil }

	var started *exec.Cmd
	app.StartCommand = func(cmd *exec.Cmd) error {
		started = cmd
		started.Process = &os.Process{Pid: 4242}
		return nil
	}

	code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--detach", "--json"})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if started == nil {
		t.Fatal("expected detached command start")
	}
	joined := strings.Join(started.Args, " ")
	if strings.Contains(joined, "--detach") || strings.Contains(joined, " -d ") {
		t.Fatalf("child still has detach flag: %s", joined)
	}
	env := strings.Join(started.Env, "\n")
	if !strings.Contains(env, envDetachWorker+"=1") || !strings.Contains(env, envDetachRunID+"=") {
		t.Fatalf("child env missing detach markers:\n%s", env)
	}
	var payload struct {
		Kind   string `json:"kind"`
		RunID  string `json:"runId"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if payload.Kind != "DetachedRun" || payload.Status != "detached" || payload.RunID == "" {
		t.Fatalf("payload = %#v", payload)
	}
	if _, err := os.Stat(filepath.Join(root, ".doppels", "detach", payload.RunID+".log")); err != nil {
		t.Fatalf("detach log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".doppels", "detach", payload.RunID+".pid")); err != nil {
		t.Fatalf("detach pid: %v", err)
	}
}
