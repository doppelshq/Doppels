package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpacesInitCreatesDirsAndManifest(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "platform")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	app, stdout, stderr := testApp(target)
	if code := app.Run([]string{"spaces", "init", "--dir", target}); code != ExitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(target, "capabilities")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "recipes")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ".doppels")); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(target, "doppels.platform.yaml")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	if !strings.Contains(out, "Root") || !strings.Contains(out, target) {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.Contains(out, "doppels apply") {
		t.Fatalf("missing apply hint in %q", out)
	}
}

func TestSpacesInitExplicitNameAndJSON(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := testApp(root)
	if code := app.Run([]string{"spaces", "init", "engineering", "--dir", root, "--json"}); code != ExitSuccess {
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
	if payload.Status != "initialized" || payload.Space != "engineering" || !payload.ManifestCreated {
		t.Fatalf("payload = %#v", payload)
	}
	if len(payload.Directories) != 3 {
		t.Fatalf("directories = %#v", payload.Directories)
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"spaces", "init", "engineering", "--dir", root, "--json"}); code != ExitSuccess {
		t.Fatalf("second exit = %d, stderr = %s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ManifestCreated {
		t.Fatalf("expected existing manifest kept: %#v", payload)
	}
}

func TestSpacesInitInvalidName(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"spaces", "init", "Bad Name", "--dir", root}); code != ExitContract {
		t.Fatalf("exit = %d, want %d, stderr = %s", code, ExitContract, stderr.String())
	}
}
