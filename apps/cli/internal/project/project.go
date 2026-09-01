package project

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"doppels.so/cli/internal/manifest"
)

// Directory is the single dot-directory that owns all Doppels artefacts.
const Directory = ".doppels"

// runsDir and lockFile live inside Directory and are gitignored.
const runsSubdir = "runs"

var ErrNotFound = errors.New("no local Space found")

// Discovery lists Capability and Recipe roots relative to a Space working tree.
type Discovery struct {
	Capabilities []string
	Recipes      []string
}

func DefaultDiscovery() Discovery {
	return Discovery{
		Capabilities: []string{".doppels/capabilities"},
		Recipes:      []string{".doppels/recipes"},
	}
}

func FindRoot(start string) (string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if isProjectRoot(current) {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", ErrNotFound
		}
		current = parent
	}
}

// Init creates the .doppels/ working tree (capabilities/, recipes/, runs/)
// and a .doppels/.gitignore that excludes runtime state.
func Init(root string) ([]string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	doppelsDir := filepath.Join(absolute, Directory)
	paths := []string{
		filepath.Join(doppelsDir, "capabilities"),
		filepath.Join(doppelsDir, "recipes"),
		filepath.Join(doppelsDir, runsSubdir),
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, err
		}
	}
	if err := writeGitignore(doppelsDir); err != nil {
		return nil, err
	}
	return paths, nil
}

// writeGitignore creates .doppels/.gitignore that ignores runtime state only.
func writeGitignore(doppelsDir string) error {
	path := filepath.Join(doppelsDir, ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil // already exists, don't overwrite
	}
	content := "# Doppels runtime state — do not commit\nruns/\nruns.db\n*.lock\n"
	return os.WriteFile(path, []byte(content), 0o644)
}

// WriteSpaceManifest creates .doppels/<space>.yaml when missing. Returns path
// and whether a new file was written.
func WriteSpaceManifest(root, space string) (string, bool, error) {
	space = strings.TrimSpace(space)
	if space == "" {
		return "", false, errors.New("space name is required")
	}
	if !validSpaceName(space) {
		return "", false, fmt.Errorf("invalid Space name %q", space)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", false, err
	}
	doppelsDir := filepath.Join(absolute, Directory)
	if err := os.MkdirAll(doppelsDir, 0o755); err != nil {
		return "", false, err
	}
	path := filepath.Join(doppelsDir, space+".space.yaml")
	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	body := fmt.Sprintf(`apiVersion: doppels.so/v1alpha1
kind: Space

metadata:
  name: %s
  displayName: %s
`, space, space)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func validSpaceName(value string) bool {
	if len(value) == 0 || len(value) > 63 {
		return false
	}
	if value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ResolveDiscovery returns default roots unless a Space stub declares discovery.
func ResolveDiscovery(root string) (Discovery, error) {
	discovery := DefaultDiscovery()
	matches, err := listSpaceManifestPaths(root)
	if err != nil {
		return Discovery{}, err
	}
	var declared *manifest.SpaceDiscovery
	var declaredFrom string
	for _, path := range matches {
		loaded, loadErr := manifest.Load(path)
		if loadErr != nil {
			continue
		}
		space, ok := loaded.Document.(*manifest.Space)
		if !ok || space.Discovery == nil {
			continue
		}
		if declared != nil {
			return Discovery{}, fmt.Errorf("multiple Space manifests declare discovery (%s and %s)", declaredFrom, path)
		}
		declared = space.Discovery
		declaredFrom = path
	}
	if declared == nil {
		return discovery, nil
	}
	if len(declared.Capabilities) > 0 {
		discovery.Capabilities = append([]string(nil), declared.Capabilities...)
	}
	if len(declared.Recipes) > 0 {
		discovery.Recipes = append([]string(nil), declared.Recipes...)
	}
	return discovery, nil
}

func Discover(root string) ([]string, error) {
	discovery, err := ResolveDiscovery(root)
	if err != nil {
		return nil, err
	}
	return DiscoverWith(root, discovery)
}

func DiscoverWith(root string, discovery Discovery) ([]string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var files []string
	seenDirs := map[string]struct{}{}
	for _, relative := range append(append([]string{}, discovery.Capabilities...), discovery.Recipes...) {
		clean, err := resolveUnderRoot(absoluteRoot, relative)
		if err != nil {
			return nil, err
		}
		if _, ok := seenDirs[clean]; ok {
			continue
		}
		seenDirs[clean] = struct{}{}
		info, err := os.Stat(clean)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", clean)
		}
		err = filepath.WalkDir(clean, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			extension := strings.ToLower(filepath.Ext(entry.Name()))
			if extension == ".yaml" || extension == ".yml" || extension == ".json" {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

func resolveUnderRoot(root, relative string) (string, error) {
	relative = strings.TrimSpace(relative)
	if relative == "" {
		return "", errors.New("discovery path must not be empty")
	}
	if strings.HasPrefix(relative, "/") || strings.Contains(relative, `\`) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("discovery path %q must be a POSIX-relative path", relative)
	}
	joined := filepath.Join(root, filepath.FromSlash(relative))
	clean := filepath.Clean(joined)
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("discovery path %q escapes Space root", relative)
	}
	return clean, nil
}

// FindSpaceManifest resolves the optional mutable configuration for the
// selected Space. Looks inside .doppels/<space>.yaml.
func FindSpaceManifest(root, space string) (string, bool, error) {
	doppelsDir := filepath.Join(root, Directory)
	var matches []string
	for _, extension := range []string{".yaml", ".yml", ".json"} {
		path := filepath.Join(doppelsDir, space+".space"+extension)
		info, err := os.Stat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return "", false, err
		case !info.Mode().IsRegular():
			return "", false, fmt.Errorf("%s is not a regular file", path)
		default:
			matches = append(matches, path)
		}
	}
	if len(matches) > 1 {
		return "", false, fmt.Errorf("multiple manifests found for Space %s", space)
	}
	if len(matches) == 0 {
		return "", false, nil
	}
	return matches[0], true, nil
}

func listSpaceManifestPaths(root string) ([]string, error) {
	var matches []string
	appendRegular := func(path string) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			matches = append(matches, path)
		}
		return nil
	}
	doppelsDir := filepath.Join(root, Directory)
	for _, extension := range []string{".yaml", ".yml", ".json"} {
		pattern := filepath.Join(doppelsDir, "*.space"+extension)
		found, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, path := range found {
			base := filepath.Base(path)
			suffix := ".space" + extension
			name := strings.TrimSuffix(base, suffix)
			if name == "" || strings.Contains(name, ".") {
				continue
			}
			if err := appendRegular(path); err != nil {
				return nil, err
			}
		}
		rootPattern := filepath.Join(root, "doppels.*"+extension)
		rootFound, err := filepath.Glob(rootPattern)
		if err != nil {
			return nil, err
		}
		for _, path := range rootFound {
			base := filepath.Base(path)
			name := strings.TrimSuffix(strings.TrimPrefix(base, "doppels."), extension)
			if name == "" || strings.Contains(name, ".") {
				continue
			}
			if err := appendRegular(path); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(matches)
	return matches, nil
}

func isProjectRoot(path string) bool {
	info, err := os.Stat(filepath.Join(path, Directory))
	if err == nil && info.IsDir() {
		return true
	}
	return hasRootSpaceStub(path)
}

func hasRootSpaceStub(path string) bool {
	for _, extension := range []string{".yaml", ".yml", ".json"} {
		pattern := filepath.Join(path, "doppels.*"+extension)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			base := filepath.Base(match)
			name := strings.TrimSuffix(strings.TrimPrefix(base, "doppels."), extension)
			if name == "" || strings.Contains(name, ".") {
				continue
			}
			info, err := os.Stat(match)
			if err == nil && info.Mode().IsRegular() {
				return true
			}
		}
	}
	return false
}

// IsWorkingTree reports whether path is a Space working tree root.
func IsWorkingTree(path string) bool {
	return isProjectRoot(path)
}

// DiscoverListenRoots resolves Project roots for `doppels node up`:
//  1. start itself is a Project → that root only
//  2. else immediate child dirs with .doppels/ → Workspace siblings
//  3. else FindRoot walk-up → single ancestor Project
func DiscoverListenRoots(start string) ([]string, error) {
	absolute, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	if isProjectRoot(absolute) {
		return []string{absolute}, nil
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return nil, err
	}
	var children []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		child := filepath.Join(absolute, name)
		if isProjectRoot(child) {
			children = append(children, child)
		}
	}
	if len(children) > 0 {
		sort.Strings(children)
		return children, nil
	}
	root, err := FindRoot(absolute)
	if err != nil {
		return nil, err
	}
	return []string{root}, nil
}
