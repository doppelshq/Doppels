// Package workspace implements the Runner's workspace registry and the
// Capability/Recipe discovery that backs v1/listWorkspaces, v1/addWorkspace,
// v1/removeWorkspace, v1/listCapabilities and v1/getCapability (RFC 001 §9).
package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"doppels.so/cli/internal/project"
)

// ErrWorkspaceNotFound is returned when a root is not a Space working tree
// (add) or is not registered (remove, scoped lookups) — RFC §11 -32003.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// registryFile is the on-disk persistence format under the Runner config
// dir. It survives Runner restarts; roots are canonical absolute paths.
type registryFile struct {
	Version int      `json:"version"`
	Roots   []string `json:"roots"`
}

// Registry persists the set of workspace roots the Runner knows about. Safe
// for concurrent use: every mutation holds the lock for the in-memory update
// and the synchronous disk write together, so readers never observe a root
// that failed to persist.
type Registry struct {
	path string

	mu    sync.RWMutex
	roots map[string]struct{}
}

// NewRegistry builds a Registry backed by path. Call Load before first use
// to pick up a previous Runner's persisted roots.
func NewRegistry(path string) *Registry {
	return &Registry{path: path, roots: make(map[string]struct{})}
}

// Load reads the persisted roots from disk. A missing file is not an error
// (first boot). Load is not safe to call concurrently with other methods.
func (r *Registry) Load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load workspace registry: %w", err)
	}
	var file registryFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return fmt.Errorf("decode workspace registry %s: %w", r.path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode workspace registry %s: trailing JSON", r.path)
	}
	if file.Version != 1 {
		return fmt.Errorf("decode workspace registry %s: unsupported version %d", r.path, file.Version)
	}
	seen := make(map[string]struct{}, len(file.Roots))
	for _, root := range file.Roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return fmt.Errorf("decode workspace registry %s: root %q is not a clean absolute path", r.path, root)
		}
		if _, duplicate := seen[root]; duplicate {
			return fmt.Errorf("decode workspace registry %s: duplicate root %q", r.path, root)
		}
		seen[root] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roots = seen
	return nil
}

// Roots returns the registered canonical roots in stable sorted order.
func (r *Registry) Roots() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sortedLocked()
}

func (r *Registry) sortedLocked() []string {
	roots := make([]string, 0, len(r.roots))
	for root := range r.roots {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots
}

// Contains reports whether canonical (already resolved via Canonicalize) is
// registered.
func (r *Registry) Contains(canonical string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.roots[canonical]
	return ok
}

// Add registers root, canonicalizing it first. Idempotent: adding an
// already-registered root (including via a symlink alias) succeeds with
// added=false and does not duplicate the entry. The root must exist and be
// a Space working tree (.doppels/ present); otherwise ErrWorkspaceNotFound.
func (r *Registry) Add(root string) (canonical string, added bool, err error) {
	canonical, err = Canonicalize(root)
	if err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrWorkspaceNotFound, err)
	}
	if !project.IsWorkingTree(canonical) {
		return "", false, fmt.Errorf("%w: %s has no .doppels/ working tree", ErrWorkspaceNotFound, canonical)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.roots[canonical]; exists {
		return canonical, false, nil
	}
	next := make(map[string]struct{}, len(r.roots)+1)
	for existing := range r.roots {
		next[existing] = struct{}{}
	}
	next[canonical] = struct{}{}
	if err := r.persistLocked(next); err != nil {
		return "", false, err
	}
	r.roots = next
	return canonical, true, nil
}

// Remove unregisters root. It never touches anything on disk beyond the
// registry file itself: workspace contents are left exactly as they are.
// Removing an unregistered root is an error (ErrWorkspaceNotFound), matching
// the RFC §11 description of -32003 ("root no registrado").
func (r *Registry) Remove(root string) (canonical string, err error) {
	canonical, err = Canonicalize(root)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkspaceNotFound, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.roots[canonical]; !exists {
		return "", fmt.Errorf("%w: %s is not registered", ErrWorkspaceNotFound, canonical)
	}
	next := make(map[string]struct{}, len(r.roots))
	for existing := range r.roots {
		if existing == canonical {
			continue
		}
		next[existing] = struct{}{}
	}
	if err := r.persistLocked(next); err != nil {
		return "", err
	}
	r.roots = next
	return canonical, nil
}

// persistLocked writes roots to disk atomically. Callers must hold r.mu.
func (r *Registry) persistLocked(roots map[string]struct{}) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("persist workspace registry: %w", err)
	}
	list := make([]string, 0, len(roots))
	for root := range roots {
		list = append(list, root)
	}
	sort.Strings(list)
	data, err := json.MarshalIndent(registryFile{Version: 1, Roots: list}, "", "  ")
	if err != nil {
		return fmt.Errorf("persist workspace registry: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(r.path), ".workspaces-*.json")
	if err != nil {
		return fmt.Errorf("persist workspace registry: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("persist workspace registry: %w", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("persist workspace registry: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("persist workspace registry: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("persist workspace registry: %w", err)
	}
	if err := os.Rename(tempPath, r.path); err != nil {
		return fmt.Errorf("persist workspace registry: %w", err)
	}
	directory, err := os.Open(filepath.Dir(r.path))
	if err != nil {
		return fmt.Errorf("persist workspace registry: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("persist workspace registry: %w", err)
	}
	return nil
}
