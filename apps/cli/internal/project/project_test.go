package project

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInitFindRootAndDiscover(t *testing.T) {
	root := t.TempDir()
	paths, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 {
		t.Fatalf("Init() returned %d paths, want 3", len(paths))
	}
	doppelsDir := filepath.Join(root, Directory)
	for _, want := range []string{
		filepath.Join(doppelsDir, "capabilities"),
		filepath.Join(doppelsDir, "recipes"),
		filepath.Join(doppelsDir, "runs"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("missing %s: %v", want, err)
		}
	}
	// .gitignore must exist
	gi := filepath.Join(doppelsDir, ".gitignore")
	if _, err := os.Stat(gi); err != nil {
		t.Fatalf("missing .doppels/.gitignore: %v", err)
	}

	nested := filepath.Join(root, "src", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	found, err := FindRoot(nested)
	if err != nil || found != root {
		t.Fatalf("FindRoot() = %q, %v; want %q", found, err, root)
	}

	capability := filepath.Join(root, Directory, "capabilities", "a.yaml")
	recipe := filepath.Join(root, Directory, "recipes", "nested", "b.yml")
	if err := os.MkdirAll(filepath.Dir(capability), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capability, []byte("kind: Capability"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(recipe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recipe, []byte("kind: Recipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, Directory, "recipes", "README.md"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, []string{capability, recipe}) {
		t.Fatalf("Discover() = %#v", files)
	}
}

func TestDefaultDiscoveryUsesUserManifestDirs(t *testing.T) {
	got := DefaultDiscovery()
	want := Discovery{
		Capabilities: []string{".doppels/capabilities"},
		Recipes:      []string{".doppels/recipes"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultDiscovery() = %#v, want %#v", got, want)
	}
}

func TestDiscoverFindsRootCapabilitiesWithoutStub(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	capPath := filepath.Join(root, Directory, "capabilities", "sync.yaml")
	if err := os.MkdirAll(filepath.Dir(capPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capPath, []byte("kind: Capability"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, []string{capPath}) {
		t.Fatalf("Discover() = %#v, want [%s]", files, capPath)
	}
}

func TestResolveDiscoveryReadsRootStub(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, "shared", "capabilities", "extra.yaml")
	if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("kind: Capability"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(root, "doppels.finance.yaml")
	if err := os.WriteFile(stub, []byte(`apiVersion: doppels.so/v1alpha1
kind: Space
metadata:
  name: finance
discovery:
  capabilities:
    - shared/capabilities
  recipes:
    - recipes
`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveDiscovery(root)
	if err != nil {
		t.Fatal(err)
	}
	want := Discovery{
		Capabilities: []string{"shared/capabilities"},
		Recipes:      []string{"recipes"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveDiscovery() = %#v, want %#v", got, want)
	}
	files, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, []string{shared}) {
		t.Fatalf("Discover() = %#v, want only shared capability", files)
	}
}

func TestDiscoverUsesSpaceDiscoveryPaths(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	sharedCap := filepath.Join(root, "shared", "capabilities", "extra.yaml")
	if err := os.MkdirAll(filepath.Dir(sharedCap), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sharedCap, []byte("kind: Capability"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaultCap := filepath.Join(root, Directory, "capabilities", "default.yaml")
	if err := os.MkdirAll(filepath.Dir(defaultCap), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultCap, []byte("kind: Capability"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Space manifest inside .doppels/
	if err := os.WriteFile(filepath.Join(root, Directory, "platform.space.yaml"), []byte(`apiVersion: doppels.so/v1alpha1
kind: Space
metadata:
  name: platform
discovery:
  capabilities:
    - shared/capabilities
  recipes:
    - .doppels/recipes
`), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, []string{sharedCap}) {
		t.Fatalf("Discover() = %#v, want only shared capability", files)
	}
}

func TestFindRootMissing(t *testing.T) {
	if _, err := FindRoot(t.TempDir()); err != ErrNotFound {
		t.Fatalf("FindRoot() error = %v, want ErrNotFound", err)
	}
}

func TestDiscoverListenRoots(t *testing.T) {
	workspace := t.TempDir()
	eng := filepath.Join(workspace, "engineering")
	fin := filepath.Join(workspace, "finance")
	if _, err := Init(eng); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(fin); err != nil {
		t.Fatal(err)
	}
	roots, err := DiscoverListenRoots(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots[0] != eng || roots[1] != fin {
		t.Fatalf("workspace roots = %#v", roots)
	}

	single, err := DiscoverListenRoots(eng)
	if err != nil || len(single) != 1 || single[0] != eng {
		t.Fatalf("project root = %#v, %v", single, err)
	}

	nested := filepath.Join(eng, "src")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	walked, err := DiscoverListenRoots(nested)
	if err != nil || len(walked) != 1 || walked[0] != eng {
		t.Fatalf("walk-up = %#v, %v", walked, err)
	}
}

func TestWriteSpaceManifest(t *testing.T) {
	root := t.TempDir()
	path, created, err := WriteSpaceManifest(root, "platform")
	if err != nil || !created {
		t.Fatalf("WriteSpaceManifest = %q, %t, %v", path, created, err)
	}
	wanted := filepath.Join(root, Directory, "platform.space.yaml")
	if path != wanted {
		t.Fatalf("path = %q, want %q", path, wanted)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: platform") {
		t.Fatalf("manifest = %s", data)
	}
	_, created, err = WriteSpaceManifest(root, "platform")
	if err != nil || created {
		t.Fatalf("second write created=%t err=%v", created, err)
	}
}

func TestIsWorkingTreeAcceptsRootSpaceStub(t *testing.T) {
	root := t.TempDir()
	if IsWorkingTree(root) {
		t.Fatal("empty dir is not a Space")
	}
	if err := os.WriteFile(filepath.Join(root, "doppels.engineering.yaml"), []byte(`apiVersion: doppels.so/v1alpha1
kind: Space
metadata:
  name: engineering
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsWorkingTree(root) {
		t.Fatal("doppels.<name>.yaml at the Space root is a working tree marker")
	}
}
