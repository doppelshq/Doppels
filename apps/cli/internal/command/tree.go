package command

import (
	"fmt"
)

type catalogTreeCounts struct {
	Caps    int `json:"caps"`
	Recipes int `json:"recipes"`
	Blocked int `json:"blocked"`
}

type catalogTreeSpace struct {
	Space        string                  `json:"space"`
	Path         string                  `json:"path"`
	Counts       catalogTreeCounts       `json:"counts"`
	Capabilities []catalogTreeCapability `json:"capabilities"`
}

type catalogTreeCapability struct {
	Name    string              `json:"name"`
	Version string              `json:"version"`
	Mode    string              `json:"mode"`
	Pin     string              `json:"pin,omitempty"`
	Source  string              `json:"source"`
	Recipes []catalogTreeRecipe `json:"recipes"`
}

type catalogTreeRecipe struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Pin     string   `json:"pin,omitempty"`
	Runtime string   `json:"runtime,omitempty"`
	Source  string   `json:"source"`
	Host    string   `json:"host,omitempty"`
	Missing []string `json:"missing,omitempty"`
}

func (app *App) runTree(arguments []string) int {
	if isHelp(arguments) {
		fmt.Fprintln(app.Stdout, "Usage: doppels tree [--json]")
		return ExitSuccess
	}
	flags := app.flagSet("tree")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "tree accepts no arguments")
		return ExitContract
	}
	index, code := app.listenLocalIndex()
	if code != ExitSuccess {
		return code
	}
	trees := index.localTrees()
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "CatalogTree", "spaces": catalogTreeSpaces(trees)})
		return ExitSuccess
	}
	writeListenLocalTrees(app.Stdout, newTermStyle(app.Stdout), trees)
	return ExitSuccess
}

func catalogTreeSpaces(trees []listenLocalTreeView) []catalogTreeSpace {
	spaces := make([]catalogTreeSpace, 0, len(trees))
	for _, tree := range trees {
		caps, recipes, blocked := listenLocalTreeStats(tree)
		space := catalogTreeSpace{
			Space:        tree.Space,
			Path:         tree.Path,
			Counts:       catalogTreeCounts{Caps: caps, Recipes: recipes, Blocked: blocked},
			Capabilities: make([]catalogTreeCapability, 0, len(tree.Capabilities)),
		}
		for _, capability := range tree.Capabilities {
			name, version := splitRevision(capability.Label)
			item := catalogTreeCapability{
				Name:    name,
				Version: version,
				Mode:    catalogTreeMode(len(capability.Recipes) > 0),
				Pin:     pinOrUnpinned(capability.Origin),
				Source:  capability.Path,
				Recipes: make([]catalogTreeRecipe, 0, len(capability.Recipes)),
			}
			for _, recipe := range capability.Recipes {
				recipeName, recipeVersion := splitRevision(recipe.Label)
				item.Recipes = append(item.Recipes, catalogTreeRecipe{
					Name:    recipeName,
					Version: recipeVersion,
					Pin:     pinOrUnpinned(recipe.Origin),
					Runtime: recipe.Runtime,
					Source:  recipe.Path,
					Host:    catalogTreeHost(recipe),
					Missing: append([]string(nil), recipe.Missing...),
				})
			}
			space.Capabilities = append(space.Capabilities, item)
		}
		spaces = append(spaces, space)
	}
	return spaces
}

func catalogTreeHost(recipe listenLocalRecipeView) string {
	if !recipe.Checked {
		return "unchecked"
	}
	if recipe.Ready {
		return "ready"
	}
	return "blocked"
}
