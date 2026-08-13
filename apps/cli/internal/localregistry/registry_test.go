package localregistry_test

import (
	"path/filepath"
	"testing"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/localregistry"
	"doppels.so/cli/internal/registryclient"
)

func TestWriteLoadAndDiff(t *testing.T) {
	root := t.TempDir()
	desired := []registryclient.Resource{{
		Kind: "Capability", SourceAuthority: "manifest",
		Revision: execution.DefinitionReference{Name: "greet", Version: "1.0.0", ManifestSHA256: "aaa"},
	}}
	if err := localregistry.Write(root, "private", desired); err != nil {
		t.Fatal(err)
	}
	if _, err := filepath.Abs(localregistry.Path(root)); err != nil {
		t.Fatal(err)
	}
	loaded, err := localregistry.Load(root)
	if err != nil || loaded.Space != "private" || len(loaded.Resources) != 1 {
		t.Fatalf("load = %#v, %v", loaded, err)
	}
	changes := localregistry.DiffChanges(nil, desired)
	if len(changes) != 1 || changes[0].Action != "create" {
		t.Fatalf("diff create = %#v", changes)
	}
	changes = localregistry.DiffChanges(desired, desired)
	if len(changes) != 1 || changes[0].Action != "noop" {
		t.Fatalf("diff noop = %#v", changes)
	}
}
