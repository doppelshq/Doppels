package command

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/projectlock"
)

type listenProjectEntry struct {
	Root     string
	Space    string
	Label    string
	Branch   string
	Worktree string
	Catalog  *manifest.Catalog
}

type listenLocalIndex struct {
	Entries []listenProjectEntry
	Merged  *manifest.Catalog
	Host    manifest.Host
}

func (app *App) listenLocalIndex() (listenLocalIndex, int) {
	workingDirectory, err := app.Getwd()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
		return listenLocalIndex{}, ExitOperational
	}
	cwd, err := filepath.Abs(workingDirectory)
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
		return listenLocalIndex{}, ExitOperational
	}
	roots, err := discoverNodeRoots(cwd)
	if err != nil {
		fmt.Fprintf(app.Stderr, "discover local Spaces: %v\n", err)
		return listenLocalIndex{}, ExitOperational
	}
	if len(roots) == 0 {
		writeNodeMissingSpace(app.Stderr, cwd)
		return listenLocalIndex{}, ExitContract
	}
	sort.Strings(roots)

	index := listenLocalIndex{Entries: make([]listenProjectEntry, 0, len(roots)), Host: app.Host}
	var allDocuments []manifest.Loaded
	for _, root := range roots {
		paths, err := project.Discover(root)
		if err != nil {
			fmt.Fprintf(app.Stderr, "discover manifests in %s: %v\n", root, err)
			return listenLocalIndex{}, ExitOperational
		}
		documents, diagnostics := load(paths)
		validation := manifest.Validate(documents, manifest.ValidationOptions{Root: root, CheckHost: false})
		diagnostics = append(diagnostics, validation.Diagnostics...)
		manifest.SortDiagnostics(diagnostics)
		if len(diagnostics) > 0 {
			for _, diagnostic := range diagnostics {
				fmt.Fprintln(app.Stderr, diagnostic.Error())
			}
			return listenLocalIndex{}, ExitContract
		}
		label := filepath.Base(root)
		if absWork, absErr := filepath.Abs(workingDirectory); absErr == nil {
			if rel, relErr := filepath.Rel(absWork, root); relErr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				label = rel
			}
		}
		spaceHint := listenSpaceHint(root)
		if store, storeErr := app.configStore(); storeErr == nil {
			if profile, profileErr := store.Profile(); profileErr == nil {
				for _, binding := range profile.Bindings {
					if binding.Path == root {
						spaceHint = binding.Space
						break
					}
				}
			}
		}
		entry := listenProjectEntry{
			Root:     root,
			Space:    spaceHint,
			Label:    label,
			Catalog:  validation.Catalog,
			Branch:   "—",
			Worktree: "—",
		}
		if branch, worktree := inspectGitWorktree(root); branch != "" || worktree != "" {
			if branch != "" {
				entry.Branch = branch
			}
			if worktree != "" {
				entry.Worktree = worktree
			}
		}
		index.Entries = append(index.Entries, entry)
		allDocuments = append(allDocuments, validation.Catalog.Documents...)
	}
	index.Merged = manifest.NewCatalog(workingDirectory, allDocuments)
	return index, ExitSuccess
}

func (index listenLocalIndex) localTrees() []listenLocalTreeView {
	trees := make([]listenLocalTreeView, 0, len(index.Entries))
	for _, entry := range index.Entries {
		path := entry.Root
		if abs, err := filepath.Abs(entry.Root); err == nil {
			path = abs
		}
		root := path
		lock, _ := projectlock.Load(entry.Root)
		trees = append(trees, listenLocalTreeView{
			Path:         path,
			Space:        entry.Space,
			Branch:       entry.Branch,
			Worktree:     entry.Worktree,
			Capabilities: listLocalCapabilityViews(entry.Catalog, root, index.Host, lock),
		})
	}
	return trees
}

func inspectGitWorktree(root string) (branch, worktree string) {
	branch = strings.TrimSpace(gitC(root, "branch", "--show-current"))
	if branch == "" {
		branch = strings.TrimSpace(gitC(root, "rev-parse", "--abbrev-ref", "HEAD"))
	}
	toplevel := strings.TrimSpace(gitC(root, "rev-parse", "--show-toplevel"))
	gitDir := strings.TrimSpace(gitC(root, "rev-parse", "--git-dir"))
	if branch == "" && toplevel == "" {
		return "", ""
	}
	if gitDir != "" && strings.Contains(filepath.ToSlash(gitDir), "/worktrees/") {
		if toplevel == "" {
			return branch, "worktree"
		}
		return branch, toplevel
	}
	if branch == "" {
		branch = "HEAD"
	}
	return branch, "primary"
}

func gitC(root string, args ...string) string {
	command := exec.Command("git", append([]string{"-C", root, "--no-optional-locks"}, args...)...)
	command.Env = append(command.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := command.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func listenSpaceHint(root string) string {
	for _, extension := range []string{".yaml", ".yml", ".json"} {
		pattern := filepath.Join(root, "doppels.*"+extension)
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		sort.Strings(matches)
		for _, match := range matches {
			base := filepath.Base(match)
			name := strings.TrimSuffix(strings.TrimPrefix(base, "doppels."), extension)
			if name == "" || strings.Contains(name, ".") {
				continue
			}
			return name
		}
	}
	return filepath.Base(root)
}

func (index listenLocalIndex) projectLabels() []string {
	labels := make([]string, 0, len(index.Entries))
	for _, entry := range index.Entries {
		labels = append(labels, entry.Label)
	}
	return labels
}

func (index listenLocalIndex) resolve(capability, space string) (string, *manifest.Catalog, error) {
	var matches []listenProjectEntry
	for _, entry := range index.Entries {
		if _, err := findCapability(entry.Catalog, capability, ""); err == nil {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return "", nil, fmt.Errorf("no local Recipe on this Node provides Capability %q; apply the Space that owns it", capability)
	}
	if space != "" {
		var spaceMatches []listenProjectEntry
		for _, entry := range matches {
			if entry.Space == space {
				spaceMatches = append(spaceMatches, entry)
			}
		}
		if len(spaceMatches) == 1 {
			return spaceMatches[0].Root, spaceMatches[0].Catalog, nil
		}
		if len(spaceMatches) > 1 {
			matches = spaceMatches
		}
	}
	if len(matches) == 1 {
		return matches[0].Root, matches[0].Catalog, nil
	}
	names := make([]string, 0, len(matches))
	for _, entry := range matches {
		names = append(names, entry.Label)
	}
	return "", nil, fmt.Errorf("Capability %q found in multiple local Spaces (%s); listen from one Space or narrow --space", capability, strings.Join(names, ", "))
}

func discoverNodeRoots(cwd string) ([]string, error) {
	if project.IsWorkingTree(cwd) {
		return []string{cwd}, nil
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		return nil, err
	}
	var children []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		child := filepath.Join(cwd, entry.Name())
		if project.IsWorkingTree(child) {
			children = append(children, child)
		}
	}
	sort.Strings(children)
	return children, nil
}

func writeNodeMissingSpace(writer io.Writer, cwd string) {
	style := newTermStyle(writer)
	path := cwd
	if abs, err := filepath.Abs(cwd); err == nil {
		path = abs
	}
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.bold("No Space working tree in this directory."))
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("cwd"), style.value(path))
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.dim("A Space is a folder with .doppels/ (capabilities/, recipes/, runtime)"))
	fmt.Fprintln(writer, "  "+style.dim("and doppels.<name>.yaml. Override paths with discovery: in that stub."))
	fmt.Fprintln(writer, "  "+style.dim("A workspace is a parent whose immediate children are Spaces."))
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.dim("cd into a Space or that workspace parent, or create one:"))
	fmt.Fprintln(writer, "    doppels init")
	fmt.Fprintln(writer)
}
