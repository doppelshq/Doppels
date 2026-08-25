package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExperimentalOnOffStatus(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	app.Environment = []string{}

	if code := app.Run([]string{"experimental", "status", "--json"}); code != ExitSuccess {
		t.Fatalf("status exit = %d stderr=%s", code, stderr.String())
	}
	var off struct {
		Experimental bool `json:"experimental"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &off); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if off.Experimental {
		t.Fatalf("expected off, got %#v", off)
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"experimental", "on", "--json"}); code != ExitSuccess {
		t.Fatalf("on exit = %d stderr=%s", code, stderr.String())
	}
	var on struct {
		Experimental bool `json:"experimental"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &on); err != nil {
		t.Fatal(err)
	}
	if !on.Experimental {
		t.Fatalf("expected on, got %#v", on)
	}
	flagFile := filepath.Join(root, ".test-config", "experimental")
	if _, err := os.Stat(flagFile); err != nil {
		t.Fatalf("missing flag file: %v", err)
	}

	stdout.Reset()
	if code := app.Run([]string{"experimental"}); code != ExitSuccess {
		t.Fatalf("default status exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "on") {
		t.Fatalf("human status = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"experimental", "off", "--json"}); code != ExitSuccess {
		t.Fatalf("off exit = %d stderr=%s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &off); err != nil {
		t.Fatal(err)
	}
	if off.Experimental {
		t.Fatalf("expected off after off, got %#v", off)
	}
}

func TestExperimentalRejectsUnknown(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	app.Environment = []string{}
	if code := app.Run([]string{"experimental", "maybe"}); code != ExitContract {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	if code := app.Run([]string{"experimental", "on", "now"}); code != ExitContract {
		t.Fatalf("extra args exit = %d", code)
	}
}

func TestExperimentalJSONFlagEitherSideOfSubcommand(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	app.Environment = []string{}
	for _, args := range [][]string{
		{"experimental", "status", "--json"},
		{"experimental", "--json", "status"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := app.Run(args); code != ExitSuccess {
			t.Fatalf("%v exit = %d stderr=%s", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"experimental": false`) {
			t.Fatalf("%v stdout = %s", args, stdout.String())
		}
	}
}

func TestExperimentalHelp(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"experimental", "--help"}); code != ExitSuccess {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: doppels experimental") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestExperimentalEnvOverridesFlagFile(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"DOPPELS_EXPERIMENTAL=1"}
	if code := app.Run([]string{"experimental", "status", "--json"}); code != ExitSuccess {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"experimental": true`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}
