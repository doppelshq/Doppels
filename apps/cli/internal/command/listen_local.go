package command

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
)

type listenProjectEntry struct {
	Root    string
	Space   string
	Label   string
	Catalog *manifest.Catalog
}

type listenLocalIndex struct {
	Entries []listenProjectEntry
	Merged  *manifest.Catalog
}

func (app *App) listenLocalIndex() (listenLocalIndex, int) {
	workingDirectory, err := app.Getwd()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
		return listenLocalIndex{}, ExitOperational
	}
	rootSet := map[string]struct{}{}
	var roots []string
	discovered, err := project.DiscoverListenRoots(workingDirectory)
	if err != nil {
		if err != project.ErrNotFound {
			fmt.Fprintf(app.Stderr, "discover local Spaces for listen: %v\n", err)
			return listenLocalIndex{}, ExitOperational
		}
	} else {
		for _, root := range discovered {
			if _, ok := rootSet[root]; ok {
				continue
			}
			rootSet[root] = struct{}{}
			roots = append(roots, root)
		}
	}
	if store, storeErr := app.configStore(); storeErr == nil {
		if profile, profileErr := store.Profile(); profileErr == nil {
			for _, binding := range profile.Bindings {
				if _, ok := rootSet[binding.Path]; ok {
					continue
				}
				if project.IsWorkingTree(binding.Path) {
					rootSet[binding.Path] = struct{}{}
					roots = append(roots, binding.Path)
				}
			}
		}
	}
	if len(roots) == 0 {
		fmt.Fprintln(app.Stderr, missingLocalSpaceMessage()+" or apply a Space on this Node first")
		return listenLocalIndex{}, ExitContract
	}
	sort.Strings(roots)

	index := listenLocalIndex{Entries: make([]listenProjectEntry, 0, len(roots))}
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
			Root:    root,
			Space:   spaceHint,
			Label:   label,
			Catalog: validation.Catalog,
		}
		index.Entries = append(index.Entries, entry)
		allDocuments = append(allDocuments, validation.Catalog.Documents...)
	}
	index.Merged = manifest.NewCatalog(workingDirectory, allDocuments)
	return index, ExitSuccess
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
