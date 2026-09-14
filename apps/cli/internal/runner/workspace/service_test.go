package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/runner/proto"
)

func newService(t *testing.T) (*Service, string) {
	t.Helper()
	base := t.TempDir()
	registry := NewRegistry(filepath.Join(base, "workspaces.json"))
	return NewService(registry, Deps{Host: manifest.OSHost{}}), base
}

func TestServiceNodeStatusUsesRealWorkspacesAndDegrades(t *testing.T) {
	service, base := newService(t)
	healthy := initRoot(t, filepath.Join(base, "healthy"))
	missing := initRoot(t, filepath.Join(base, "missing"))
	if _, _, err := service.AddWorkspace(healthy); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.AddWorkspace(missing); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}

	status := service.NodeStatus(proto.NodeStatus{State: "online", StartedAt: "2026-09-14T10:00:00Z"})
	if status.State != "degraded" {
		t.Fatalf("State = %q, want degraded", status.State)
	}
	if len(status.Workspaces) != 2 || status.Workspaces[1].Health != "missingRoot" {
		t.Fatalf("Workspaces = %+v, want real summaries including missing root", status.Workspaces)
	}
}

func TestServiceScopedCapabilitiesRejectMissingRegisteredRoot(t *testing.T) {
	for _, remove := range []struct {
		name string
		path func(string) string
	}{
		{name: "root", path: func(root string) string { return root }},
		{name: "doppels", path: func(root string) string { return filepath.Join(root, project.Directory) }},
	} {
		t.Run(remove.name, func(t *testing.T) {
			service, base := newService(t)
			root := initRoot(t, filepath.Join(base, "space-a"))
			if _, _, err := service.AddWorkspace(root); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(remove.path(root)); err != nil {
				t.Fatal(err)
			}
			if got := service.ListWorkspaces(); len(got) != 1 || got[0].Health != "missingRoot" {
				t.Fatalf("ListWorkspaces = %+v, want retained missingRoot", got)
			}
			if _, err := service.ListCapabilities(root); !errors.Is(err, ErrWorkspaceNotFound) {
				t.Fatalf("ListCapabilities error = %v, want ErrWorkspaceNotFound", err)
			}
			if _, err := service.GetCapability(root, "cap-a", ""); !errors.Is(err, ErrWorkspaceNotFound) {
				t.Fatalf("GetCapability error = %v, want ErrWorkspaceNotFound", err)
			}
		})
	}
}

func TestServiceAddListRemoveWorkspace(t *testing.T) {
	service, base := newService(t)
	root := initRoot(t, filepath.Join(base, "space-a"))

	summary, added, err := service.AddWorkspace(root)
	if err != nil || !added {
		t.Fatalf("AddWorkspace: summary=%+v added=%v err=%v", summary, added, err)
	}
	if got := service.ListWorkspaces(); len(got) != 1 || got[0].Root != summary.Root {
		t.Fatalf("ListWorkspaces() = %+v", got)
	}

	canonical, err := service.RemoveWorkspace(root)
	if err != nil {
		t.Fatalf("RemoveWorkspace: %v", err)
	}
	if canonical != summary.Root {
		t.Fatalf("RemoveWorkspace root = %q, want %q", canonical, summary.Root)
	}
	if got := service.ListWorkspaces(); len(got) != 0 {
		t.Fatalf("ListWorkspaces() after remove = %+v", got)
	}
}

func TestServiceAddReportsInvalidManifestHealth(t *testing.T) {
	service, base := newService(t)
	root := initRoot(t, filepath.Join(base, "invalid"))
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "broken.yaml"), "kind: Capability\nmetadata: {}\n")

	summary, added, err := service.AddWorkspace(root)
	if err != nil || !added || summary.Health != "invalidManifests" {
		t.Fatalf("AddWorkspace = %+v, %v, %v; want registered invalidManifests summary", summary, added, err)
	}
}

func TestServiceDoesNotExposeEscapingOrDanglingManifestSymlinks(t *testing.T) {
	for _, tt := range []struct {
		name   string
		target func(base, root string) string
		write  bool
	}{
		{
			name: "external",
			target: func(base, _ string) string {
				return filepath.Join(base, "outside.yaml")
			},
			write: true,
		},
		{
			name: "dangling",
			target: func(_, root string) string {
				return filepath.Join(root, "missing.yaml")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service, base := newService(t)
			root := initRoot(t, filepath.Join(base, "space-a"))
			target := tt.target(base, root)
			if tt.write {
				writeFile(t, target, manualCapabilityFixture)
			}
			link := filepath.Join(root, project.Directory, "capabilities", "linked.yaml")
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}

			summary, added, err := service.AddWorkspace(root)
			if err != nil || !added || summary.Health != healthInvalidManifests || summary.Capabilities != 0 {
				t.Fatalf("AddWorkspace = %+v, %v, %v; want invalid workspace with no capabilities", summary, added, err)
			}
			listed, err := service.ListCapabilities(root)
			if err != nil || len(listed) != 0 {
				t.Fatalf("ListCapabilities = %+v, %v; escaped manifest must be excluded", listed, err)
			}
			if _, err := service.GetCapability(root, "answer-question", "1.0.0"); !errors.Is(err, ErrCapabilityNotFound) {
				t.Fatalf("GetCapability error = %v, want escaped manifest hidden", err)
			}
		})
	}
}

func TestServiceListCapabilitiesAggregatesAcrossWorkspaces(t *testing.T) {
	service, base := newService(t)
	rootA := initRoot(t, filepath.Join(base, "space-a"))
	rootB := initRoot(t, filepath.Join(base, "space-b"))
	writeFile(t, filepath.Join(rootA, ".doppels", "capabilities", "a.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: cap-a, version: 1.0.0}
inputs: {}
outputs: {ok: {type: string}}
`)
	writeFile(t, filepath.Join(rootB, ".doppels", "capabilities", "b.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: cap-b, version: 1.0.0}
inputs: {}
outputs: {ok: {type: string}}
`)
	if _, _, err := service.AddWorkspace(rootA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.AddWorkspace(rootB); err != nil {
		t.Fatal(err)
	}
	if got := service.ListWorkspaces(); len(got) != 2 || got[0].Capabilities+got[1].Capabilities != 2 {
		t.Fatalf("ListWorkspaces() = %+v, want 2 registered workspaces with 2 capabilities", got)
	}

	all, err := service.ListCapabilities("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListCapabilities(\"\") = %+v, want 2 entries", all)
	}

	scoped, err := service.ListCapabilities(rootA)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].Name != "cap-a" {
		t.Fatalf("ListCapabilities(rootA) = %+v", scoped)
	}
}

func TestServiceListCapabilitiesUnknownWorkspace(t *testing.T) {
	service, base := newService(t)
	if _, err := service.ListCapabilities(filepath.Join(base, "never-added")); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("err = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestServiceGetCapabilityNotFound(t *testing.T) {
	service, base := newService(t)
	root := initRoot(t, filepath.Join(base, "space-a"))
	if _, _, err := service.AddWorkspace(root); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetCapability(root, "does-not-exist", ""); !errors.Is(err, ErrCapabilityNotFound) {
		t.Fatalf("err = %v, want ErrCapabilityNotFound", err)
	}
}

func TestServiceGetCapabilityRequiresWorkspace(t *testing.T) {
	service, _ := newService(t)
	if _, err := service.GetCapability("", "cap-a", ""); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("err = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestServiceGetCapabilityAmbiguousVersion(t *testing.T) {
	service, base := newService(t)
	root := initRoot(t, filepath.Join(base, "space-a"))
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "v1.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: cap-a, version: 1.0.0}
inputs: {}
outputs: {ok: {type: string}}
`)
	writeFile(t, filepath.Join(root, ".doppels", "capabilities", "v2.yaml"), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: cap-a, version: 2.0.0}
inputs: {}
outputs: {ok: {type: string}}
`)
	if _, _, err := service.AddWorkspace(root); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetCapability(root, "cap-a", ""); !errors.Is(err, ErrCapabilityAmbiguous) {
		t.Fatalf("err = %v, want ErrCapabilityAmbiguous", err)
	}
	result, err := service.GetCapability(root, "cap-a", "2.0.0")
	if err != nil {
		t.Fatalf("GetCapability with explicit version: %v", err)
	}
	if result.Summary.Version != "2.0.0" {
		t.Fatalf("Summary.Version = %q, want 2.0.0", result.Summary.Version)
	}
	if result.Manifest == nil || result.Manifest.Metadata.Version != "2.0.0" {
		t.Fatalf("Manifest = %+v", result.Manifest)
	}
	if result.Runs != 0 {
		t.Fatalf("Runs = %d, want 0 (no run subsystem in this slice)", result.Runs)
	}
}

func TestServiceDuplicateCapabilityRevisionIsInvalidAndExcluded(t *testing.T) {
	service, base := newService(t)
	root := initRoot(t, filepath.Join(base, "space-a"))
	for _, filename := range []string{"first.yaml", "second.yaml"} {
		writeFile(t, filepath.Join(root, ".doppels", "capabilities", filename), `apiVersion: doppels.so/v1alpha1
kind: Capability
metadata: {name: cap-a, version: 1.0.0}
inputs: {}
outputs: {ok: {type: string}}
`)
	}
	summary, _, err := service.AddWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Health != "invalidManifests" || summary.Capabilities != 0 {
		t.Fatalf("summary = %+v, want duplicate manifests excluded and unhealthy", summary)
	}
	capabilities, err := service.ListCapabilities(root)
	if err != nil || len(capabilities) != 0 {
		t.Fatalf("ListCapabilities = %+v, %v; want duplicate revisions excluded", capabilities, err)
	}
	if _, err := service.GetCapability(root, "cap-a", "1.0.0"); !errors.Is(err, ErrCapabilityNotFound) {
		t.Fatalf("GetCapability duplicate revision error = %v, want not found in usable catalog", err)
	}
}

func TestServiceConcurrentClients(t *testing.T) {
	service, base := newService(t)
	var roots []string
	for i := 0; i < 4; i++ {
		roots = append(roots, initRoot(t, filepath.Join(base, "space-"+string(rune('a'+i)))))
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		root := roots[i%len(roots)]
		wg.Add(4)
		go func() { defer wg.Done(); _, _, _ = service.AddWorkspace(root) }()
		go func() { defer wg.Done(); _, _ = service.RemoveWorkspace(root) }()
		go func() { defer wg.Done(); _ = service.ListWorkspaces() }()
		go func() { defer wg.Done(); _, _ = service.ListCapabilities("") }()
	}
	wg.Wait()
}
