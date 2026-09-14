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
	"doppels.so/cli/internal/runner/proto"
)

// ErrWorkspaceNotFound is returned when a root is not a Space working tree
// (add) or is not registered (remove, scoped lookups) — RFC §11 -32003.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// PostCommitWarning reports that a mutation reached its logical commit point
// but lost the guarantee that the containing directory entry is durable. The
// returned mutation result is authoritative for the running process and the
// Registry remains fail-stop until restart.
type PostCommitWarning struct {
	cause error
}

func (w *PostCommitWarning) Error() string { return w.cause.Error() }
func (w *PostCommitWarning) Unwrap() error { return w.cause }

// registryFile is the on-disk persistence format under the Runner config
// dir. It survives Runner restarts; roots are canonical absolute paths.
type registryFile struct {
	Version int      `json:"version"`
	Roots   []string `json:"roots"`
}

// Registry persists the set of workspace roots the Runner knows about. Safe
// for concurrent use: every mutation holds the lock across its install commit
// point and the matching in-memory update. A post-install durability error
// reconciles memory to the installed file and leaves the registry fail-stop.
type Registry struct {
	path  string
	files registryFiles

	mu     sync.RWMutex
	roots  map[string]struct{}
	failed error
}

type directorySyncer interface {
	Sync() error
	Close() error
}

type registryFiles struct {
	rename        func(oldPath, newPath string) error
	openDirectory func(path string) (directorySyncer, error)
}

// NewRegistry builds a Registry backed by path. Call Load before first use
// to pick up a previous Runner's persisted roots.
func NewRegistry(path string) *Registry {
	return &Registry{
		path:  path,
		roots: make(map[string]struct{}),
		files: registryFiles{
			rename: os.Rename,
			openDirectory: func(path string) (directorySyncer, error) {
				return os.Open(path)
			},
		},
	}
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
	canonical, err := proto.CanonicalJSON(data)
	if err != nil {
		return fmt.Errorf("decode workspace registry %s: %w", r.path, err)
	}
	var file struct {
		Version int       `json:"version"`
		Roots   *[]string `json:"roots"`
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
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
	if file.Roots == nil {
		return fmt.Errorf("decode workspace registry %s: roots must be a non-null array", r.path)
	}
	seen := make(map[string]struct{}, len(*file.Roots))
	for _, root := range *file.Roots {
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

// Degraded reports whether a post-commit durability failure has left the
// Registry fail-stop until restart.
func (r *Registry) Degraded() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.failed != nil
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return "", false, fmt.Errorf("workspace registry is fail-stop after persistence failure: %w", r.failed)
	}
	if _, exists := r.roots[canonical]; exists {
		return canonical, false, nil
	}
	if !project.IsWorkingTree(canonical) {
		return "", false, fmt.Errorf("%w: %s has no .doppels/ working tree", ErrWorkspaceNotFound, canonical)
	}
	next := make(map[string]struct{}, len(r.roots)+1)
	for existing := range r.roots {
		next[existing] = struct{}{}
	}
	next[canonical] = struct{}{}
	installed, err := r.persistLocked(next)
	if installed {
		r.roots = next
	}
	if err != nil {
		if installed {
			r.failed = err
			return canonical, true, &PostCommitWarning{cause: err}
		}
		return "", false, err
	}
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
	if r.failed != nil {
		return "", fmt.Errorf("workspace registry is fail-stop after persistence failure: %w", r.failed)
	}
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
	installed, err := r.persistLocked(next)
	if installed {
		r.roots = next
	}
	if err != nil {
		if installed {
			r.failed = err
			return canonical, &PostCommitWarning{cause: err}
		}
		return "", err
	}
	return canonical, nil
}

// persistLocked writes roots to disk atomically. installed is true once
// rename has made the new file observable, even if directory durability then
// fails. Callers must hold r.mu.
func (r *Registry) persistLocked(roots map[string]struct{}) (installed bool, err error) {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return false, fmt.Errorf("persist workspace registry: %w", err)
	}
	list := make([]string, 0, len(roots))
	for root := range roots {
		list = append(list, root)
	}
	sort.Strings(list)
	data, err := json.MarshalIndent(registryFile{Version: 1, Roots: list}, "", "  ")
	if err != nil {
		return false, fmt.Errorf("persist workspace registry: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(r.path), ".workspaces-*.json")
	if err != nil {
		return false, fmt.Errorf("persist workspace registry: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return false, fmt.Errorf("persist workspace registry: %w", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return false, fmt.Errorf("persist workspace registry: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return false, fmt.Errorf("persist workspace registry: %w", err)
	}
	if err := temp.Close(); err != nil {
		return false, fmt.Errorf("persist workspace registry: %w", err)
	}
	if err := r.files.rename(tempPath, r.path); err != nil {
		return false, fmt.Errorf("persist workspace registry: %w", err)
	}
	directory, err := r.files.openDirectory(filepath.Dir(r.path))
	if err != nil {
		return true, fmt.Errorf("persist workspace registry after install: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return true, fmt.Errorf("persist workspace registry after install: %w", err)
	}
	return true, nil
}
