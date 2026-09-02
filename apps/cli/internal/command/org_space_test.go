package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doppels.so/cli/internal/project"
)

func TestWarnMissingSpaceManifestPointsUsersToDoppelsInit(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, project.Directory), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout := &bytes.Buffer{}
	app := &App{
		Stdout: stdout,
		Stderr: &bytes.Buffer{},
		Getwd:  func() (string, error) { return root, nil },
		Host:   unavailableHost{},
	}
	app.warnMissingSpaceManifest("platform")
	got := stdout.String()
	if !strings.Contains(got, "doppels init") {
		t.Fatalf("expected hint to mention `doppels init`, got: %s", got)
	}
	if strings.Contains(got, "doppels spaces init") {
		t.Fatalf("stale `doppels spaces init` reference in hint: %s", got)
	}
}
