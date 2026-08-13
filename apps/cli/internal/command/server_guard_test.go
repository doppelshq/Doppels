package command

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/configstore"
)

func TestWriteServerTokenMismatch(t *testing.T) {
	var buf bytes.Buffer
	writeServerTokenMismatch(&buf, "https://saved.example", "https://target.example")
	out := buf.String()
	for _, want := range []string{
		"Login does not match --server",
		"https://saved.example",
		"https://target.example",
		"doppels logout",
		"doppels login --server https://target.example",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestShareKeepsSavedTokenPairedWithItsServer(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	// Isolate from host DOPPELS_* so Session() is what drives the guard.
	app.Environment = []string{}
	store := configstore.New(filepath.Join(root, ".test-config"))
	if err := store.Login("https://profile.example.test", "saved-token", time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := app.Run([]string{"share", "capability/anything", "--server", "https://other.example.test"}); code != ExitContract {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, "Login does not match --server") {
		t.Fatalf("stderr = %s", out)
	}
	if !strings.Contains(out, "https://profile.example.test") || !strings.Contains(out, "https://other.example.test") {
		t.Fatalf("stderr missing servers: %s", out)
	}
}
