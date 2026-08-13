package command

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/projectlock"
)

type validateItem struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	DisplayName string   `json:"displayName,omitempty"`
	Path        string   `json:"path,omitempty"`
	Status      string   `json:"status"`
	Digest      string   `json:"digest,omitempty"`
	Provides    []string `json:"provides,omitempty"`
}

func buildValidateItems(root string, documents []manifest.Loaded, catalog *manifest.Catalog, lock *projectlock.File) []validateItem {
	locked := map[string]string{}
	if lock != nil {
		for _, entry := range lock.Resources {
			locked[lockResourceKey(entry.Kind, entry.Revision.Name, entry.Revision.Version)] = entry.Revision.ManifestSHA256
		}
	}

	items := make([]validateItem, 0, len(documents))
	if catalog != nil {
		for _, name := range sortedKeys(catalog.Capabilities) {
			for _, definition := range catalog.Capabilities[name] {
				ref := execution.ReferenceCapability(definition)
				items = append(items, validateItem{
					Kind:        "Capability",
					Name:        ref.Name,
					Version:     ref.Version,
					DisplayName: validateTitle(definition.Value.Metadata),
					Path:        relPath(root, definition.Source.Path),
					Status:      validateLockStatus(locked, "Capability", ref.Name, ref.Version, ref.ManifestSHA256),
					Digest:      ref.ManifestSHA256,
				})
			}
		}
		for _, name := range sortedKeys(catalog.Recipes) {
			for _, definition := range catalog.Recipes[name] {
				ref := execution.ReferenceRecipe(definition)
				provides := append([]string(nil), definition.Value.Provides...)
				sort.Strings(provides)
				items = append(items, validateItem{
					Kind:        "Recipe",
					Name:        ref.Name,
					Version:     ref.Version,
					DisplayName: validateTitle(definition.Value.Metadata),
					Path:        relPath(root, definition.Source.Path),
					Status:      validateLockStatus(locked, "Recipe", ref.Name, ref.Version, ref.ManifestSHA256),
					Digest:      ref.ManifestSHA256,
					Provides:    provides,
				})
			}
		}
	}
	for _, document := range documents {
		space, ok := document.Document.(*manifest.Space)
		if !ok {
			continue
		}
		title := space.Metadata.DisplayName
		if title == "" {
			title = space.Metadata.Summary
		}
		items = append(items, validateItem{
			Kind:        "Space",
			Name:        space.Metadata.Name,
			DisplayName: title,
			Path:        relPath(root, document.Path),
			Status:      "valid",
			Digest:      document.SHA256,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return kindOrder(items[i].Kind) < kindOrder(items[j].Kind)
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Version < items[j].Version
	})
	return items
}

func validateLockStatus(locked map[string]string, kind, name, version, digest string) string {
	if len(locked) == 0 {
		return "valid"
	}
	previous, ok := locked[lockResourceKey(kind, name, version)]
	if !ok {
		return "new"
	}
	if previous == digest {
		return "unchanged"
	}
	return "changed"
}

func lockResourceKey(kind, name, version string) string {
	return strings.ToLower(kind) + "/" + name + "@" + version
}

func kindOrder(kind string) int {
	switch kind {
	case "Capability":
		return 0
	case "Recipe":
		return 1
	case "Space":
		return 2
	default:
		return 9
	}
}

func relPath(root, path string) string {
	if root == "" {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func discoveryFolders(root string, paths []string) []string {
	seen := map[string]struct{}{}
	folders := make([]string, 0)
	for _, path := range paths {
		rel := relPath(root, filepath.Dir(path))
		if rel == "." || rel == "" {
			rel = "."
		}
		top, _, _ := strings.Cut(rel, "/")
		if top == "" {
			continue
		}
		if _, ok := seen[top]; ok {
			continue
		}
		seen[top] = struct{}{}
		folders = append(folders, top+"/")
	}
	sort.Strings(folders)
	return folders
}

func validateTitle(meta manifest.Metadata) string {
	if meta.DisplayName != "" {
		return meta.DisplayName
	}
	return meta.Summary
}

func writeValidateReport(writer io.Writer, root string, paths []string, documents []manifest.Loaded, items []validateItem) {
	style := newTermStyle(writer)
	rows := [][]string{{style.dim("RESOURCE"), style.dim("STATUS"), style.dim("TITLE")}}
	rows = append(rows, validateHierarchyRows(style, items)...)
	writeAlignedColumns(writer, rows)
	fmt.Fprintln(writer)

	fmt.Fprintf(writer, "  %s  %s\n", style.boldGreen(checkMark(style)), style.bold("Validated"))
	if folders := discoveryFolders(root, paths); len(folders) > 0 {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Paths"), style.dim(strings.Join(folders, "  ")))
	}
	caps, recipes, spaces := definitionCounts(documents)
	parts := []string{
		pluralCount(caps, "Capability", "Capabilities"),
		pluralCount(recipes, "Recipe", "Recipes"),
	}
	if spaces > 0 {
		parts = append(parts, pluralCount(spaces, "Space", "Spaces"))
	}
	parts = append(parts, pluralCount(len(documents), "manifest", "manifests"))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Count"), strings.Join(parts, " · "))
}

func validateHierarchyRows(style termStyle, items []validateItem) [][]string {
	var capabilities, recipes, spaces []validateItem
	for _, item := range items {
		switch item.Kind {
		case "Capability":
			capabilities = append(capabilities, item)
		case "Recipe":
			recipes = append(recipes, item)
		case "Space":
			spaces = append(spaces, item)
		}
	}

	byCapability := map[string][]validateItem{}
	linked := map[string]struct{}{}
	capabilityNames := map[string]struct{}{}
	for _, capability := range capabilities {
		capabilityNames[capability.Name] = struct{}{}
	}
	for _, recipe := range recipes {
		for _, provided := range recipe.Provides {
			if _, ok := capabilityNames[provided]; !ok {
				continue
			}
			byCapability[provided] = append(byCapability[provided], recipe)
			linked[validateResourceRef(recipe)] = struct{}{}
		}
	}

	rows := make([][]string, 0, len(items)+len(spaces))
	for _, space := range spaces {
		rows = append(rows, validateItemRow(style, "", space))
	}
	for _, capability := range capabilities {
		rows = append(rows, validateItemRow(style, "", capability))
		children := dedupeValidateItems(byCapability[capability.Name])
		sort.Slice(children, func(i, j int) bool {
			if children[i].Name != children[j].Name {
				return children[i].Name < children[j].Name
			}
			return children[i].Version < children[j].Version
		})
		for index, recipe := range children {
			prefix := "├─ "
			if index == len(children)-1 {
				prefix = "└─ "
			}
			rows = append(rows, validateItemRow(style, style.dim(prefix), recipe))
		}
	}
	for _, recipe := range recipes {
		if _, ok := linked[validateResourceRef(recipe)]; ok {
			continue
		}
		rows = append(rows, validateItemRow(style, "", recipe))
	}
	return rows
}

func dedupeValidateItems(items []validateItem) []validateItem {
	seen := map[string]struct{}{}
	out := make([]validateItem, 0, len(items))
	for _, item := range items {
		key := validateResourceRef(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func validateItemRow(style termStyle, prefix string, item validateItem) []string {
	title := item.DisplayName
	if title == "" {
		title = item.Path
	}
	return []string{
		prefix + validateResourceRef(item),
		colorValidateStatus(style, item.Status),
		title,
	}
}

func pluralCount(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func validateResourceRef(item validateItem) string {
	ref := strings.ToLower(item.Kind) + "/" + item.Name
	if item.Version != "" {
		ref += "@" + item.Version
	}
	return ref
}

func colorValidateStatus(style termStyle, status string) string {
	switch status {
	case "unchanged":
		return style.dim(status)
	case "new":
		return style.cyan(status)
	case "changed":
		return style.yellow(status)
	default:
		return style.green(status)
	}
}
