package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"doppels.so/cli/internal/project"
)

func initRoot(t *testing.T, dir string) string {
	t.Helper()
	if _, err := project.Init(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRegistryAddPersistsAcrossRestart(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	storePath := filepath.Join(base, "workspaces.json")

	registry := NewRegistry(storePath)
	if err := registry.Load(); err != nil {
		t.Fatal(err)
	}
	canonical, added, err := registry.Add(root)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !added {
		t.Fatal("expected added=true on first add")
	}

	restarted := NewRegistry(storePath)
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	roots := restarted.Roots()
	if len(roots) != 1 || roots[0] != canonical {
		t.Fatalf("Roots() after restart = %v, want [%s]", roots, canonical)
	}
}

func TestRegistryAddIsIdempotent(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))

	first, added1, err := registry.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	if !added1 {
		t.Fatal("expected added=true on first add")
	}
	second, added2, err := registry.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	if added2 {
		t.Fatal("expected added=false on repeat add")
	}
	if first != second {
		t.Fatalf("canonical root changed: %s vs %s", first, second)
	}
	if len(registry.Roots()) != 1 {
		t.Fatalf("Roots() = %v, want a single entry", registry.Roots())
	}
}

func TestRegistryAddRemainsIdempotentAfterRootDisappears(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	path := filepath.Join(base, "workspaces.json")
	registry := NewRegistry(path)
	canonical, _, err := registry.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	got, added, err := registry.Add(root)
	if err != nil || added || got != canonical {
		t.Fatalf("Add missing registered root = %q, %v, %v; want idempotent %q", got, added, err, canonical)
	}

	restarted := NewRegistry(path)
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	got, added, err = restarted.Add(root)
	if err != nil || added || got != canonical {
		t.Fatalf("Add missing root after restart = %q, %v, %v; want idempotent %q", got, added, err, canonical)
	}
}

func TestRegistryAddRemainsIdempotentAfterDoppelsDisappears(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	canonical, _, err := registry.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, project.Directory)); err != nil {
		t.Fatal(err)
	}
	got, added, err := registry.Add(root)
	if err != nil || added || got != canonical {
		t.Fatalf("Add registered root without .doppels = %q, %v, %v", got, added, err)
	}
}

func TestRegistryAddRemainsIdempotentThroughDanglingSymlink(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	alias := filepath.Join(base, "space-a-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	canonical, _, err := registry.Add(alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	got, added, err := registry.Add(alias)
	if err != nil || added || got != canonical {
		t.Fatalf("Add through dangling registered alias = %q, %v, %v", got, added, err)
	}
}

func TestRegistryAddRejectsMissingDoppelsDir(t *testing.T) {
	base := t.TempDir()
	notAProject := filepath.Join(base, "plain-dir")
	if err := os.MkdirAll(notAProject, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	if _, _, err := registry.Add(notAProject); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Add() err = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestRegistryAddRejectsNonexistentRoot(t *testing.T) {
	base := t.TempDir()
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	if _, _, err := registry.Add(filepath.Join(base, "does-not-exist")); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Add() err = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestRegistryAddResolvesSymlinkAlias(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	alias := filepath.Join(base, "space-a-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	canonicalFromRoot, _, err := registry.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalFromAlias, added, err := registry.Add(alias)
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Fatal("expected alias add to resolve to the same, already-registered root")
	}
	if canonicalFromRoot != canonicalFromAlias {
		t.Fatalf("alias resolved to %s, want %s", canonicalFromAlias, canonicalFromRoot)
	}
	if len(registry.Roots()) != 1 {
		t.Fatalf("Roots() = %v, want a single entry", registry.Roots())
	}
}

func TestRegistryRemoveIsNotDestructive(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	canonical, _, err := registry.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Remove(root); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(registry.Roots()) != 0 {
		t.Fatalf("Roots() = %v, want empty after remove", registry.Roots())
	}
	if info, err := os.Stat(filepath.Join(canonical, ".doppels")); err != nil || !info.IsDir() {
		t.Fatalf(".doppels directory removed from disk: %v", err)
	}
}

func TestRegistryRemoveUnknownRootErrors(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	if _, err := registry.Remove(root); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Remove() err = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestRegistryRemoveAfterRootDeletedFromDisk(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	canonical, _, err := registry.Add(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Remove(root); err != nil {
		t.Fatalf("Remove of a root deleted from disk should still succeed: %v", err)
	}
	if len(registry.Roots()) != 0 {
		t.Fatalf("Roots() = %v, want empty", registry.Roots())
	}
}

func TestRegistryRemoveThroughDanglingSymlinkAlias(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	alias := filepath.Join(base, "space-a-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	if _, _, err := registry.Add(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Remove(alias); err != nil {
		t.Fatalf("Remove through dangling symlink alias: %v", err)
	}
}

func TestRegistryLoadRejectsUnsafePersistence(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unsupported version", body: `{"version":2,"roots":[]}`},
		{name: "relative root", body: `{"version":1,"roots":["relative/space"]}`},
		{name: "unknown field", body: `{"version":1,"roots":[],"surprise":true}`},
		{name: "trailing value", body: `{"version":1,"roots":[]} {}`},
		{name: "missing roots", body: `{"version":1}`},
		{name: "null roots", body: `{"version":1,"roots":null}`},
		{name: "roots is object", body: `{"version":1,"roots":{}}`},
		{name: "duplicate roots key", body: `{"version":1,"roots":[],"roots":["/tmp/other"]}`},
		{name: "duplicate version key", body: `{"version":2,"version":1,"roots":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "workspaces.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := NewRegistry(path).Load(); err == nil {
				t.Fatalf("Load accepted unsafe registry: %s", tt.body)
			}
		})
	}
}

func TestRegistryIdempotenceAndRemovalSurviveRestart(t *testing.T) {
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	path := filepath.Join(base, "workspaces.json")
	first := NewRegistry(path)
	canonical, added, err := first.Add(root)
	if err != nil || !added {
		t.Fatalf("first Add = %q, %v, %v", canonical, added, err)
	}
	restarted := NewRegistry(path)
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	if got, added, err := restarted.Add(root); err != nil || added || got != canonical {
		t.Fatalf("idempotent Add after restart = %q, %v, %v", got, added, err)
	}
	if _, err := restarted.Remove(root); err != nil {
		t.Fatal(err)
	}
	empty := NewRegistry(path)
	if err := empty.Load(); err != nil {
		t.Fatal(err)
	}
	if roots := empty.Roots(); len(roots) != 0 {
		t.Fatalf("Roots after remove + restart = %v", roots)
	}
}

func TestRegistryRootsAreAbsoluteAndSortedAfterRestart(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "workspaces.json")
	for _, name := range []string{"z-space", "a-space"} {
		initRoot(t, filepath.Join(base, name))
	}
	body, err := json.Marshal(registryFile{Version: 1, Roots: []string{filepath.Join(base, "z-space"), filepath.Join(base, "a-space")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(path)
	if err := registry.Load(); err != nil {
		t.Fatal(err)
	}
	roots := registry.Roots()
	if len(roots) != 2 || roots[0] != filepath.Join(base, "a-space") || !filepath.IsAbs(roots[0]) {
		t.Fatalf("Roots = %v, want absolute sorted roots", roots)
	}
}

type failingDirectory struct {
	syncErr error
}

func (d failingDirectory) Sync() error  { return d.syncErr }
func (d failingDirectory) Close() error { return nil }

func TestRegistryPersistenceCommitPoint(t *testing.T) {
	injected := errors.New("injected persistence failure")
	tests := []struct {
		name      string
		inject    func(*Registry)
		installed bool
	}{
		{
			name: "rename",
			inject: func(registry *Registry) {
				registry.files.rename = func(string, string) error { return injected }
			},
		},
		{
			name: "open directory after rename",
			inject: func(registry *Registry) {
				registry.files.openDirectory = func(string) (directorySyncer, error) { return nil, injected }
			},
			installed: true,
		},
		{
			name: "sync directory after rename",
			inject: func(registry *Registry) {
				registry.files.openDirectory = func(string) (directorySyncer, error) {
					return failingDirectory{syncErr: injected}, nil
				}
			},
			installed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			root := initRoot(t, filepath.Join(base, "space-a"))
			path := filepath.Join(base, "workspaces.json")
			registry := NewRegistry(path)
			tt.inject(registry)
			canonical, err := Canonicalize(root)
			if err != nil {
				t.Fatal(err)
			}

			got, added, err := registry.Add(root)
			if !errors.Is(err, injected) {
				t.Fatalf("Add error = %v, want injected error", err)
			}
			var warning *PostCommitWarning
			if tt.installed {
				if got != canonical || !added || !errors.As(err, &warning) {
					t.Fatalf("Add = %q, %v, %v; want committed result and typed warning", got, added, err)
				}
			} else if got != "" || added || errors.As(err, &warning) {
				t.Fatalf("Add = %q, %v, %v; pre-commit failure must not report a commit", got, added, err)
			}
			if registry.Contains(canonical) != tt.installed {
				t.Fatalf("in-memory installed = %v, want %v", registry.Contains(canonical), tt.installed)
			}

			restarted := NewRegistry(path)
			loadErr := restarted.Load()
			if tt.installed {
				if loadErr != nil || !restarted.Contains(canonical) {
					t.Fatalf("restart = contains %v, error %v; want installed state", restarted.Contains(canonical), loadErr)
				}
				if _, err := registry.Remove(root); !errors.Is(err, injected) {
					t.Fatalf("mutation after uncertain durability = %v, want fail-stop error", err)
				}
			} else if loadErr != nil || restarted.Contains(canonical) {
				t.Fatalf("restart = contains %v, error %v; want old state", restarted.Contains(canonical), loadErr)
			}
		})
	}
}

func TestRegistryRemoveReturnsCommittedResultAfterPostRenameFailure(t *testing.T) {
	injected := errors.New("injected directory sync failure")
	base := t.TempDir()
	root := initRoot(t, filepath.Join(base, "space-a"))
	path := filepath.Join(base, "workspaces.json")
	registry := NewRegistry(path)
	canonical, added, err := registry.Add(root)
	if err != nil || !added {
		t.Fatalf("seed Add = %q, %v, %v", canonical, added, err)
	}
	registry.files.openDirectory = func(string) (directorySyncer, error) {
		return failingDirectory{syncErr: injected}, nil
	}

	got, err := registry.Remove(root)
	var warning *PostCommitWarning
	if got != canonical || !errors.Is(err, injected) || !errors.As(err, &warning) {
		t.Fatalf("Remove = %q, %v; want committed result and typed warning", got, err)
	}
	if registry.Contains(canonical) {
		t.Fatal("committed removal remained in memory")
	}
	restarted := NewRegistry(path)
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	if restarted.Contains(canonical) {
		t.Fatal("committed removal remained on disk after restart")
	}
	if _, _, err := registry.Add(root); !errors.Is(err, injected) {
		t.Fatalf("mutation after uncertain durability = %v, want fail-stop error", err)
	}
}

func TestRegistryConcurrentAddRemoveList(t *testing.T) {
	base := t.TempDir()
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	var roots []string
	for i := 0; i < 5; i++ {
		roots = append(roots, initRoot(t, filepath.Join(base, "space-"+string(rune('a'+i)))))
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		root := roots[i%len(roots)]
		go func() { defer wg.Done(); _, _, _ = registry.Add(root) }()
		go func() { defer wg.Done(); _, _ = registry.Remove(root) }()
		go func() { defer wg.Done(); _ = registry.Roots() }()
	}
	wg.Wait()
}
