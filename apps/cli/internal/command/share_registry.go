package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/registryclient"
)

func (app *App) capabilityFromRegistry(apiToken, server, capabilityRef string) (manifest.CapabilityDefinition, error) {
	kind, reference, ok := strings.Cut(strings.TrimSpace(capabilityRef), "/")
	if !ok || kind != "capability" || reference == "" {
		return manifest.CapabilityDefinition{}, fmt.Errorf("resource must use capability/<name>[@version]")
	}
	name, version, _ := strings.Cut(reference, "@")
	store, err := app.configStore()
	if err != nil {
		return manifest.CapabilityDefinition{}, err
	}
	scope, err := store.Context()
	if err != nil || !scope.Valid() || scope.Space == "" || scope.IsLocal() {
		return manifest.CapabilityDefinition{}, errors.New("cloud share from registry requires org use + space use on a non-local Space")
	}
	client, err := app.registryClient(server)
	if err != nil {
		return manifest.CapabilityDefinition{}, err
	}
	items, err := client.Capabilities(app.context(), apiToken, scope.Organization, scope.Space)
	if err != nil {
		return manifest.CapabilityDefinition{}, err
	}
	var match *registryclient.RegistryDefinition
	for index := range items {
		item := &items[index]
		if item.Name != name {
			continue
		}
		if version != "" && item.Revision.Version != version {
			continue
		}
		match = item
		break
	}
	if match == nil {
		return manifest.CapabilityDefinition{}, fmt.Errorf("Capability %q not found in %s", reference, scope.String())
	}
	raw, err := json.Marshal(match.Revision.Manifest)
	if err != nil {
		return manifest.CapabilityDefinition{}, err
	}
	var capability manifest.Capability
	if err := json.Unmarshal(raw, &capability); err != nil {
		return manifest.CapabilityDefinition{}, fmt.Errorf("decode registry Capability: %w", err)
	}
	if capability.Metadata.Name == "" {
		capability.Metadata.Name = match.Name
	}
	if capability.Metadata.Version == "" {
		capability.Metadata.Version = match.Revision.Version
	}
	return manifest.CapabilityDefinition{
		Value: &capability,
		Source: manifest.Loaded{
			Path:      "registry://" + scope.String() + "/capability/" + match.Name,
			Directory: ".",
			SHA256:    match.Revision.ManifestSHA256,
			Document:  &capability,
		},
	}, nil
}
