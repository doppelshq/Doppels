package workspace

import (
	"errors"
	"os"
	"path/filepath"
)

// Canonicalize resolves path to an absolute, symlink-free form so aliases of
// the same workspace (relative paths, symlinks) collapse to one registry
// entry. A root that no longer exists on disk (e.g. removed after being
// registered) still canonicalizes deterministically via filepath.Clean, so a
// later Remove of the same alias keeps matching.
func Canonicalize(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// EvalSymlinks cannot resolve a final symlink once its target has
			// disappeared. Read that link directly so an alias used to add a
			// workspace can still remove the persisted canonical target later.
			if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
				target, readErr := os.Readlink(abs)
				if readErr != nil {
					return "", readErr
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(abs), target)
				}
				return Canonicalize(target)
			}
			return filepath.Clean(abs), nil
		}
		return "", err
	}
	return resolved, nil
}
