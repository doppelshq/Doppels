package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/projectlock"
	"doppels.so/cli/internal/runner/proto"
)

// Deps injects the host probe used for Recipe readiness checks (RFC §8
// CapabilitySummary.readiness). Tests substitute a fake manifest.Host; the
// Runner always wires manifest.OSHost{}.
type Deps struct {
	Host manifest.Host
}

// health values for WorkspaceSummary (RFC §8).
const (
	healthOK               = "ok"
	healthInvalidManifests = "invalidManifests"
	healthMissingRoot      = "missingRoot"
)

// loadedWorkspace is the intermediate state shared by WorkspaceSummary and
// CapabilitySummary construction: discover once, derive both views from it.
type loadedWorkspace struct {
	Catalog *manifest.Catalog
	Lock    *projectlock.File
	Health  string
}

// loadWorkspaceState discovers and validates root's manifests through the
// real project/manifest pipeline (project.Discover + manifest.Load +
// manifest.Validate) — no parallel parser. Host checks are excluded here:
// workspace health reflects manifest validity, not per-Recipe readiness.
func loadWorkspaceState(root string, deps Deps) loadedWorkspace {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() || !project.IsWorkingTree(root) {
		return loadedWorkspace{Health: healthMissingRoot}
	}
	paths, err := project.Discover(root)
	if err != nil {
		return loadedWorkspace{Health: healthInvalidManifests}
	}
	documents, diagnostics := loadManifests(paths)
	validation := manifest.Validate(documents, manifest.ValidationOptions{Root: root, CheckHost: false})
	diagnostics = append(diagnostics, validation.Diagnostics...)

	lock, lockErr := projectlock.Load(root)
	health := healthOK
	if len(diagnostics) > 0 || lockErr != nil {
		health = healthInvalidManifests
	}
	return loadedWorkspace{Catalog: validation.Catalog, Lock: lock, Health: health}
}

// loadManifests loads every discovered manifest path through manifest.Load,
// collecting decode failures as diagnostics instead of aborting: one broken
// file should not hide the rest of the workspace.
func loadManifests(paths []string) ([]manifest.Loaded, []manifest.Diagnostic) {
	documents := make([]manifest.Loaded, 0, len(paths))
	var diagnostics []manifest.Diagnostic
	for _, path := range paths {
		loaded, err := manifest.Load(path)
		if err != nil {
			diagnostics = append(diagnostics, manifest.Diagnostic{Source: path, Code: "manifest.decode", Message: err.Error()})
			continue
		}
		documents = append(documents, loaded)
	}
	return documents, diagnostics
}

// BuildWorkspaceSummary computes the RFC §8 WorkspaceSummary for root.
func BuildWorkspaceSummary(root string, deps Deps) proto.WorkspaceSummary {
	state := loadWorkspaceState(root, deps)
	summary := proto.WorkspaceSummary{
		Root:      root,
		Space:     spaceHint(root),
		GitBranch: gitBranch(root),
		Health:    state.Health,
	}
	if state.Catalog != nil {
		caps, recipes := 0, 0
		for _, document := range state.Catalog.Documents {
			switch document.Document.(type) {
			case *manifest.Capability:
				caps++
			case *manifest.Recipe:
				recipes++
			}
		}
		summary.Capabilities = caps
		summary.Recipes = recipes
	}
	return summary
}

// buildCapabilitySummaries computes one CapabilitySummary per discovered
// Capability revision (every name@version pair), in a stable order:
// name, then version ascending.
func buildCapabilitySummaries(root string, catalog *manifest.Catalog, lock *projectlock.File, deps Deps) []proto.CapabilitySummary {
	if catalog == nil {
		return nil
	}
	space := spaceHint(root)
	names := make([]string, 0, len(catalog.Capabilities))
	for name := range catalog.Capabilities {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []proto.CapabilitySummary
	for _, name := range names {
		definitions := append([]manifest.CapabilityDefinition(nil), catalog.Capabilities[name]...)
		sort.Slice(definitions, func(i, j int) bool {
			left, right := definitions[i], definitions[j]
			if left.Value.Metadata.Version != right.Value.Metadata.Version {
				return left.Value.Metadata.Version < right.Value.Metadata.Version
			}
			if left.Source.SHA256 != right.Source.SHA256 {
				return left.Source.SHA256 < right.Source.SHA256
			}
			return left.Source.Path < right.Source.Path
		})
		recipes := append([]manifest.RecipeDefinition(nil), catalog.RecipesForCapability(name)...)
		sort.Slice(recipes, func(i, j int) bool {
			left, right := recipes[i], recipes[j]
			if left.Value.Metadata.Name != right.Value.Metadata.Name {
				return left.Value.Metadata.Name < right.Value.Metadata.Name
			}
			if left.Value.Metadata.Version != right.Value.Metadata.Version {
				return left.Value.Metadata.Version < right.Value.Metadata.Version
			}
			if left.Source.SHA256 != right.Source.SHA256 {
				return left.Source.SHA256 < right.Source.SHA256
			}
			return left.Source.Path < right.Source.Path
		})
		for _, definition := range definitions {
			if definition.Value == nil {
				continue
			}
			version := definition.Value.Metadata.Version
			summary := proto.CapabilitySummary{
				Name:           name,
				Version:        version,
				Workspace:      root,
				Space:          space,
				ManifestSHA256: definition.Source.SHA256,
				Pin:            resourceOrigin(lock, "Capability", name, version, definition.Source.SHA256),
				Readiness:      []proto.RequirementCheck{},
			}
			if len(recipes) == 0 {
				summary.Runtime = "none"
			} else {
				recipe := recipes[0]
				summary.Runtime = recipe.Value.Runtime
				summary.Recipe = &proto.RecipeReference{
					Name:           recipe.Value.Metadata.Name,
					Version:        recipe.Value.Metadata.Version,
					ManifestSHA256: recipe.Source.SHA256,
				}
				summary.Readiness = buildReadiness(recipe.Value, catalog.Root, deps.Host)
			}
			out = append(out, summary)
		}
	}
	return out
}

// buildReadiness reports one RequirementCheck per required command (RFC §8:
// "readiness: { command, ok }[] // CheckRequires"), reusing
// manifest.CheckRequires as the single source of truth for what is missing.
func buildReadiness(recipe *manifest.Recipe, catalogRoot string, host manifest.Host) []proto.RequirementCheck {
	checks := []proto.RequirementCheck{}
	if host == nil || recipe.Runtime != "shell" || recipe.Requires == nil {
		return checks
	}
	missing := make(map[string]struct{})
	for _, label := range manifest.CheckRequires(recipe, catalogRoot, host) {
		missing[label] = struct{}{}
	}
	for _, command := range recipe.Requires.Commands {
		_, notReady := missing["command "+command.Name]
		checks = append(checks, proto.RequirementCheck{Command: command.Name, OK: !notReady})
	}
	return checks
}

// resourceOrigin classifies a resource's pin state against doppels.lock:
// unpinned (no matching lock entry), pinned (digest matches) or stale
// (locked revision's digest diverged from what's on disk).
func resourceOrigin(lock *projectlock.File, kind, name, version, digest string) string {
	if lock == nil {
		return "unpinned"
	}
	for _, entry := range lock.Resources {
		if entry.Kind != kind || entry.Revision.Name != name || entry.Revision.Version != version {
			continue
		}
		if digest != "" && entry.Revision.ManifestSHA256 != digest {
			return "stale"
		}
		return "pinned"
	}
	return "unpinned"
}

// spaceHint resolves the local Space name through the real manifest loader.
// Both the current .doppels/<name>.space.yaml layout and the legacy root
// doppels.<name>.yaml stub are accepted by internal/project.
func spaceHint(root string) string {
	var paths []string
	for _, extension := range []string{".yaml", ".yml", ".json"} {
		for _, pattern := range []string{
			filepath.Join(root, project.Directory, "*.space"+extension),
			filepath.Join(root, "doppels.*"+extension),
		} {
			matches, err := filepath.Glob(pattern)
			if err == nil {
				paths = append(paths, matches...)
			}
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		loaded, err := manifest.Load(path)
		if err != nil {
			continue
		}
		space, ok := loaded.Document.(*manifest.Space)
		if ok && space.Metadata.Name != "" {
			return space.Metadata.Name
		}
	}
	return filepath.Base(root)
}

// gitBranch reports root's current git branch, or nil when root is not a
// git working tree or HEAD is detached.
func gitBranch(root string) *string {
	branch := strings.TrimSpace(gitOutput(root, "branch", "--show-current"))
	if branch == "" {
		return nil
	}
	return &branch
}

func gitOutput(root string, args ...string) string {
	command := exec.Command("git", append([]string{"-C", root, "--no-optional-locks"}, args...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat")
	out, err := command.Output()
	if err != nil {
		return ""
	}
	return string(out)
}
