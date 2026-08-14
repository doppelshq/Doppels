// Package localregistry persists the offline (org local) control-plane snapshot
// under .doppels/ for preview/apply without Cloud.
package localregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"doppels.so/cli/internal/registryclient"
)

const Filename = "local-registry.json"

type File struct {
	Version   int                       `json:"version"`
	Space     string                    `json:"space"`
	Resources []registryclient.Resource `json:"resources"`
}

func Path(root string) string {
	return filepath.Join(root, ".doppels", Filename)
}

func Load(root string) (File, error) {
	path := Path(root)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{Version: 1}, nil
		}
		return File{}, err
	}
	var document File
	if err := json.Unmarshal(data, &document); err != nil {
		return File{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if document.Version == 0 {
		document.Version = 1
	}
	return document, nil
}

func Write(root string, space string, resources []registryclient.Resource) error {
	if err := os.MkdirAll(filepath.Join(root, ".doppels"), 0o700); err != nil {
		return err
	}
	document := File{Version: 1, Space: space, Resources: resources}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := Path(root)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// DiffChanges builds a simple create/noop plan against the previous local registry.
func DiffChanges(previous, desired []registryclient.Resource) []registryclient.Change {
	prev := map[string]registryclient.Resource{}
	for _, resource := range previous {
		prev[resourceKey(resource)] = resource
	}
	changes := make([]registryclient.Change, 0, len(desired))
	for _, resource := range desired {
		key := resourceKey(resource)
		existing, ok := prev[key]
		action := "create"
		if ok {
			if existing.Revision.ManifestSHA256 == resource.Revision.ManifestSHA256 &&
				existing.Revision.Version == resource.Revision.Version {
				action = "noop"
			} else {
				action = "update"
			}
		}
		changes = append(changes, registryclient.Change{
			Action:  action,
			Kind:    resource.Kind,
			Name:    resource.Revision.Name,
			Version: resource.Revision.Version,
		})
	}
	return changes
}

func resourceKey(resource registryclient.Resource) string {
	return resource.Kind + "/" + resource.Revision.Name
}
