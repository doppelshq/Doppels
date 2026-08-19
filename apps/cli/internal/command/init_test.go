package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitDefaultCreatesLocalSpace(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"init", "--dir", root}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	for _, sub := range []string{"capabilities", "recipes", ".doppels"} {
		if _, err := os.Stat(filepath.Join(root, sub)); err != nil {
			t.Fatalf("missing %s: %v", sub, err)
		}
	}
	manifest := filepath.Join(root, "doppels.private.yaml")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("missing private manifest: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "Root") || !strings.Contains(out, root) {
		t.Fatalf("stdout missing Root: %q", out)
	}
	if !strings.Contains(out, "Space") || !strings.Contains(out, "private") {
		t.Fatalf("stdout missing Space: %q", out)
	}
}

func TestInitExplicitName(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"init", "platform", "--dir", root}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "doppels.platform.yaml")); err != nil {
		t.Fatalf("missing platform manifest: %v", err)
	}
	if !strings.Contains(stdout.String(), "platform") {
		t.Fatalf("stdout missing space name: %q", stdout.String())
	}
}

func TestInitIdempotentExistingManifest(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"init", "myspace", "--dir", root}); code != ExitSuccess {
		t.Fatalf("first exit = %d, stderr = %s", code, stderr.String())
	}
	app2, stdout2, stderr2 := testApp(root)
	if code := app2.Run([]string{"init", "myspace", "--dir", root}); code != ExitSuccess {
		t.Fatalf("second exit = %d, stderr = %s", code, stderr2.String())
	}
	if !strings.Contains(stdout2.String(), "Kept") {
		t.Fatalf("expected 'Kept' on second run: %q", stdout2.String())
	}
}

func TestInitJSON(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"init", "eng", "--dir", root, "--json"}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var payload struct {
		Status          string   `json:"status"`
		Root            string   `json:"root"`
		Space           string   `json:"space"`
		Manifest        string   `json:"manifest"`
		ManifestCreated bool     `json:"manifestCreated"`
		Directories     []string `json:"directories"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if payload.Status != "initialized" || payload.Space != "eng" || !payload.ManifestCreated {
		t.Fatalf("payload = %#v", payload)
	}
	if len(payload.Directories) != 3 {
		t.Fatalf("directories = %#v", payload.Directories)
	}
}

func TestInitInvalidName(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"init", "Bad Name", "--dir", root}); code != ExitContract {
		t.Fatalf("exit = %d, want %d, stderr = %s", code, ExitContract, stderr.String())
	}
}

func TestInitTooManyArgs(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"init", "foo", "bar", "--dir", root}); code != ExitContract {
		t.Fatalf("exit = %d, want %d, stderr = %s", code, ExitContract, stderr.String())
	}
}
