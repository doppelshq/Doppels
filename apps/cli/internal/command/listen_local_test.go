package command

import (
	"os"
	"path/filepath"
	"testing"

	"doppels.so/cli/internal/manifest"
)

func TestListenLocalIndexResolveBySpace(t *testing.T) {
	t.Parallel()
	engCatalog := manifest.NewCatalog("eng", []manifest.Loaded{
		{Document: &manifest.Capability{Metadata: manifest.Metadata{Name: "create-preview", Version: "1.0.0"}}},
	})
	finCatalog := manifest.NewCatalog("fin", []manifest.Loaded{
		{Document: &manifest.Capability{Metadata: manifest.Metadata{Name: "create-preview", Version: "1.0.0"}}},
		{Document: &manifest.Capability{Metadata: manifest.Metadata{Name: "sync-invoices", Version: "1.0.0"}}},
	})
	index := listenLocalIndex{Entries: []listenProjectEntry{
		{Root: "/ws/engineering", Space: "engineering", Label: "engineering", Catalog: engCatalog},
		{Root: "/ws/finance", Space: "finance", Label: "finance", Catalog: finCatalog},
	}}
	root, catalog, err := index.resolve("create-preview", "engineering")
	if err != nil || root != "/ws/engineering" || catalog != engCatalog {
		t.Fatalf("resolve = %q %#v %v", root, catalog, err)
	}
	root, catalog, err = index.resolve("create-preview", "finance")
	if err != nil || root != "/ws/finance" || catalog != finCatalog {
		t.Fatalf("space prefer = %q %#v %v", root, catalog, err)
	}
	_, _, err = index.resolve("missing", "")
	if err == nil {
		t.Fatal("expected missing capability error")
	}
}

func TestListenSpaceHint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "doppels.engineering.yaml")
	if err := os.WriteFile(path, []byte("kind: Space\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := listenSpaceHint(dir); got != "engineering" {
		t.Fatalf("hint = %q", got)
	}
}
