package workspace

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"doppels.so/cli/internal/listener"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/runner/proto"
)

var (
	// ErrCapabilityNotFound maps to RFC 001 §11 -32004.
	ErrCapabilityNotFound = errors.New("capability not found")
	// ErrCapabilityAmbiguous requires callers to include metadata.version when
	// more than one revision with the requested name exists.
	ErrCapabilityAmbiguous = errors.New("capability version is ambiguous")
)

// Capability contains the complete getCapability response payload.
type Capability struct {
	Summary  proto.CapabilitySummary            `json:"summary"`
	Manifest *manifest.Capability               `json:"manifest"`
	Inputs   map[string]manifest.InputContract  `json:"inputs"`
	Outputs  map[string]manifest.OutputContract `json:"outputs"`
	Runs     int                                `json:"runs"`
}

// Execution is a fully resolved invocation source from the same validated
// catalog used by listCapabilities/getCapability.
type Execution struct {
	Root            string
	Space           string
	Capability      manifest.CapabilityDefinition
	Recipe          *manifest.RecipeDefinition
	RecipeDirectory string
	StalePin        bool
}

// Service is the concurrency-safe workspace and local catalog facade used by
// the Runner's JSON-RPC handlers. mutationMu establishes a single commit order
// for mutations; discovery builds an immutable catalog per request.
type Service struct {
	registry      *Registry
	deps          Deps
	mutationMu    sync.Mutex
	eventSequence uint64
}

func NewService(registry *Registry, deps Deps) *Service {
	return &Service{registry: registry, deps: deps}
}

func (s *Service) AddWorkspace(root string) (proto.WorkspaceSummary, bool, error) {
	s.mutationMu.Lock()
	canonical, added, err := s.registry.Add(root)
	s.mutationMu.Unlock()
	if _, committed := asPostCommitWarning(err); err != nil && !committed {
		return proto.WorkspaceSummary{}, false, err
	}
	return BuildWorkspaceSummary(canonical, s.deps), added, err
}

func (s *Service) RemoveWorkspace(root string) (string, error) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	return s.registry.Remove(root)
}

func (s *Service) addWorkspaceForEvent(root string) (proto.WorkspaceSummary, bool, uint64, error) {
	s.mutationMu.Lock()
	canonical, added, err := s.registry.Add(root)
	var sequence uint64
	_, committed := asPostCommitWarning(err)
	if (err == nil || committed) && added {
		s.eventSequence++
		sequence = s.eventSequence
	}
	s.mutationMu.Unlock()
	if err != nil && !committed {
		return proto.WorkspaceSummary{}, false, 0, err
	}
	return BuildWorkspaceSummary(canonical, s.deps), added, sequence, err
}

func (s *Service) removeWorkspaceForEvent(root string) (string, uint64, error) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	canonical, err := s.registry.Remove(root)
	_, committed := asPostCommitWarning(err)
	if err != nil && !committed {
		return "", 0, err
	}
	s.eventSequence++
	return canonical, s.eventSequence, err
}

func asPostCommitWarning(err error) (*PostCommitWarning, bool) {
	var warning *PostCommitWarning
	ok := errors.As(err, &warning)
	return warning, ok
}

func (s *Service) ListWorkspaces() []proto.WorkspaceSummary {
	roots := s.registry.Roots()
	workspaces := make([]proto.WorkspaceSummary, 0, len(roots))
	for _, root := range roots {
		workspaces = append(workspaces, BuildWorkspaceSummary(root, s.deps))
	}
	return workspaces
}

// NodeStatus fills a server-owned status envelope with current workspace
// summaries. A persisted workspace that cannot be loaded degrades the node;
// an already-degraded state is never promoted here.
func (s *Service) NodeStatus(status proto.NodeStatus) proto.NodeStatus {
	status.Workspaces = s.ListWorkspaces()
	if s.registry.Degraded() {
		status.State = "degraded"
	}
	for _, workspace := range status.Workspaces {
		if workspace.Health != healthOK {
			status.State = "degraded"
			break
		}
	}
	return status
}

func (s *Service) ListCapabilities(workspace string) ([]proto.CapabilitySummary, error) {
	roots, err := s.rootsFor(workspace)
	if err != nil {
		return nil, err
	}
	var capabilities []proto.CapabilitySummary
	for _, root := range roots {
		state := loadWorkspaceState(root, s.deps)
		if workspace != "" && state.Health == healthMissingRoot {
			return nil, fmt.Errorf("%w: %s is registered but missing", ErrWorkspaceNotFound, root)
		}
		capabilities = append(capabilities, buildCapabilitySummaries(root, state.Catalog, state.Lock, s.deps)...)
	}
	sort.Slice(capabilities, func(i, j int) bool {
		left, right := capabilities[i], capabilities[j]
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.Version != right.Version {
			return left.Version < right.Version
		}
		if left.Workspace != right.Workspace {
			return left.Workspace < right.Workspace
		}
		return left.ManifestSHA256 < right.ManifestSHA256
	})
	if capabilities == nil {
		capabilities = []proto.CapabilitySummary{}
	}
	return capabilities, nil
}

func (s *Service) GetCapability(workspace, name, version string) (Capability, error) {
	if workspace == "" {
		return Capability{}, fmt.Errorf("%w: workspace is required", ErrWorkspaceNotFound)
	}
	roots, err := s.rootsFor(workspace)
	if err != nil {
		return Capability{}, err
	}
	root := roots[0]
	state := loadWorkspaceState(root, s.deps)
	if state.Health == healthMissingRoot {
		return Capability{}, fmt.Errorf("%w: %s is registered but missing", ErrWorkspaceNotFound, root)
	}
	if state.Catalog == nil {
		return Capability{}, fmt.Errorf("%w: %s", ErrCapabilityNotFound, name)
	}
	definitions := state.Catalog.Capabilities[name]
	var matches []manifest.CapabilityDefinition
	for _, definition := range definitions {
		if definition.Value != nil && (version == "" || definition.Value.Metadata.Version == version) {
			matches = append(matches, definition)
		}
	}
	if len(matches) == 0 {
		return Capability{}, fmt.Errorf("%w: %s@%s", ErrCapabilityNotFound, name, version)
	}
	if len(matches) > 1 {
		return Capability{}, fmt.Errorf("%w: %s has %d revisions; include version", ErrCapabilityAmbiguous, name, len(matches))
	}
	definition := matches[0]
	summaries := buildCapabilitySummaries(root, state.Catalog, state.Lock, s.deps)
	for _, summary := range summaries {
		if summary.Name == name && summary.Version == definition.Value.Metadata.Version && summary.ManifestSHA256 == definition.Source.SHA256 {
			return Capability{
				Summary:  summary,
				Manifest: definition.Value,
				Inputs:   definition.Value.Inputs,
				Outputs:  definition.Value.Outputs,
				Runs:     0,
			}, nil
		}
	}
	return Capability{}, fmt.Errorf("%w: %s@%s", ErrCapabilityNotFound, name, version)
}

// ResolveExecution resolves Capability name[@version] and the optional Recipe
// using the established manifest Catalog rules. A missing Recipe is the valid
// manual-without-Recipe case; ambiguity still requires an explicit selection.
func (s *Service) ResolveExecution(workspace, capabilityReference, recipeReference string) (Execution, error) {
	roots, err := s.rootsFor(workspace)
	if err != nil {
		return Execution{}, err
	}
	if len(roots) != 1 {
		return Execution{}, fmt.Errorf("%w: workspace is required", ErrWorkspaceNotFound)
	}
	root := roots[0]
	state := loadWorkspaceState(root, s.deps)
	if state.Health == healthMissingRoot {
		return Execution{}, fmt.Errorf("%w: %s is registered but missing", ErrWorkspaceNotFound, root)
	}
	if state.Catalog == nil {
		return Execution{}, fmt.Errorf("%w: %s", ErrCapabilityNotFound, capabilityReference)
	}
	if strings.Count(capabilityReference, "@") > 1 {
		return Execution{}, fmt.Errorf("%w: malformed reference %q", ErrCapabilityNotFound, capabilityReference)
	}
	name, version, _ := strings.Cut(capabilityReference, "@")
	if name == "" || (strings.Contains(capabilityReference, "@") && version == "") {
		return Execution{}, fmt.Errorf("%w: malformed reference %q", ErrCapabilityNotFound, capabilityReference)
	}
	capability, err := listener.FindCapability(state.Catalog, name, version)
	if err != nil {
		return Execution{}, fmt.Errorf("%w: %v", ErrCapabilityNotFound, err)
	}
	resolved := Execution{Root: root, Space: spaceHint(root), Capability: capability}
	recipe, recipeErr := state.Catalog.ResolveRecipe(name, recipeReference)
	switch {
	case recipeErr == nil:
		resolved.Recipe = &recipe
		resolved.RecipeDirectory = filepath.Dir(recipe.Source.Path)
	case errors.Is(recipeErr, manifest.ErrRecipeNotFound) && recipeReference == "":
		// Capability without Recipe is a durable manual Run.
	case recipeErr != nil:
		return Execution{}, recipeErr
	}
	resolved.StalePin = resourceOrigin(state.Lock, "Capability", name, capability.Value.Metadata.Version, capability.Source.SHA256) == "stale"
	if resolved.Recipe != nil && resourceOrigin(state.Lock, "Recipe", resolved.Recipe.Value.Metadata.Name, resolved.Recipe.Value.Metadata.Version, resolved.Recipe.Source.SHA256) == "stale" {
		resolved.StalePin = true
	}
	return resolved, nil
}

func (s *Service) rootsFor(workspace string) ([]string, error) {
	if workspace == "" {
		return s.registry.Roots(), nil
	}
	canonical, err := Canonicalize(workspace)
	if err != nil || !s.registry.Contains(canonical) {
		return nil, fmt.Errorf("%w: %s is not registered", ErrWorkspaceNotFound, workspace)
	}
	return []string{canonical}, nil
}
