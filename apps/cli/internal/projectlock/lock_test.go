package projectlock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doppels.so/cli/internal/execution"
)

func entry(digest string) Entry {
	return Entry{Kind: "Capability", SourceAuthority: "manifest", Revision: execution.DefinitionReference{
		Name: "greet", Version: "1.0.0", ManifestSHA256: digest,
		Schema: execution.SchemaReference{ID: "https://doppels.so/schema", SHA256: strings.Repeat("b", 64)},
	}}
}

func TestPrepareDoesNotPublishUntilCommitAndCanAbort(t *testing.T) {
	root := t.TempDir()
	wanted := New([]Entry{entry(strings.Repeat("a", 64))})
	pending, err := Prepare(root, wanted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, Filename)); !os.IsNotExist(err) {
		t.Fatalf("lock was published before commit: %v", err)
	}
	if _, err := os.Stat(pending.temporaryPath); err != nil {
		t.Fatalf("prepared lock is missing: %v", err)
	}
	if err := pending.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pending.temporaryPath); !os.IsNotExist(err) {
		t.Fatalf("aborted temporary lock still exists: %v", err)
	}

	pending, err = Prepare(root, wanted)
	if err != nil {
		t.Fatal(err)
	}
	if err := pending.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err != nil {
		t.Fatal(err)
	}
}

func TestLockRoundTripAndConflict(t *testing.T) {
	root := t.TempDir()
	wanted := New([]Entry{entry(strings.Repeat("a", 64))})
	if err := Write(root, wanted); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(loaded, wanted); err != nil {
		t.Fatal(err)
	}
	changed := New([]Entry{entry(strings.Repeat("c", 64))})
	if err := Verify(loaded, changed); err == nil || !strings.Contains(err.Error(), "version bump") {
		t.Fatalf("Verify error = %v", err)
	}
}
