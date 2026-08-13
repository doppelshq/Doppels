package manifest

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrRecipeNotFound  = errors.New("no compatible Recipe found")
	ErrRecipeAmbiguous = errors.New("multiple compatible Recipes found")
)

// Catalog keeps immutable definitions together with their local source. The
// source directory and digest are deliberately retained: execution resolves
// relative files from the Recipe directory and records the exact revision.
type Catalog struct {
	Root         string
	Documents    []Loaded
	Capabilities map[string][]CapabilityDefinition
	Recipes      map[string][]RecipeDefinition
}

type CapabilityDefinition struct {
	Source Loaded
	Value  *Capability
}

type RecipeDefinition struct {
	Source Loaded
	Value  *Recipe
}

func NewCatalog(root string, documents []Loaded) *Catalog {
	catalog := &Catalog{
		Root:         root,
		Documents:    append([]Loaded(nil), documents...),
		Capabilities: make(map[string][]CapabilityDefinition),
		Recipes:      make(map[string][]RecipeDefinition),
	}
	for _, loaded := range documents {
		switch document := loaded.Document.(type) {
		case *Capability:
			catalog.Capabilities[document.Metadata.Name] = append(
				catalog.Capabilities[document.Metadata.Name],
				CapabilityDefinition{Source: loaded, Value: document},
			)
		case *Recipe:
			catalog.Recipes[document.Metadata.Name] = append(
				catalog.Recipes[document.Metadata.Name],
				RecipeDefinition{Source: loaded, Value: document},
			)
		}
	}
	for name := range catalog.Capabilities {
		sort.Slice(catalog.Capabilities[name], func(i, j int) bool {
			return catalog.Capabilities[name][i].Value.Metadata.Version < catalog.Capabilities[name][j].Value.Metadata.Version
		})
	}
	for name := range catalog.Recipes {
		sort.Slice(catalog.Recipes[name], func(i, j int) bool {
			return catalog.Recipes[name][i].Value.Metadata.Version < catalog.Recipes[name][j].Value.Metadata.Version
		})
	}
	return catalog
}

func (c *Catalog) RecipesForCapability(capability string) []RecipeDefinition {
	var matches []RecipeDefinition
	for _, revisions := range c.Recipes {
		for _, definition := range revisions {
			for _, provided := range definition.Value.Provides {
				if provided == capability {
					matches = append(matches, definition)
					break
				}
			}
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		left, right := matches[i].Value.Metadata, matches[j].Value.Metadata
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.Version < right.Version
	})
	return matches
}

// ResolveRecipe encodes the MVP selection rule without executing anything:
// one compatible Recipe is deterministic; multiple require an explicit name.
func (c *Catalog) ResolveRecipe(capability, explicit string) (RecipeDefinition, error) {
	matches := c.RecipesForCapability(capability)
	if explicit == "" {
		if len(matches) == 0 {
			return RecipeDefinition{}, ErrRecipeNotFound
		}
		if len(matches) > 1 {
			return RecipeDefinition{}, fmt.Errorf("%w for %s; select one explicitly", ErrRecipeAmbiguous, capability)
		}
		return matches[0], nil
	}

	name, version, _ := strings.Cut(explicit, "@")
	var selected []RecipeDefinition
	for _, match := range matches {
		if match.Value.Metadata.Name == name && (version == "" || match.Value.Metadata.Version == version) {
			selected = append(selected, match)
		}
	}
	if len(selected) == 0 {
		return RecipeDefinition{}, fmt.Errorf("%w: %s does not provide %s", ErrRecipeNotFound, explicit, capability)
	}
	if len(selected) > 1 {
		return RecipeDefinition{}, fmt.Errorf("%w for %s; include @version", ErrRecipeAmbiguous, explicit)
	}
	return selected[0], nil
}
