package command

import (
	"errors"
	"fmt"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/runsync"
)

func (app *App) maybeFlushOutbox() {
	workingDirectory, err := app.Getwd()
	if err != nil {
		return
	}
	root, err := project.FindRoot(workingDirectory)
	if err != nil {
		return
	}
	store, err := app.configStore()
	if err != nil {
		return
	}
	session, err := store.Session()
	if err != nil {
		return
	}
	scope := session.Profile.Context
	if scope.Organization == "" || scope.Space == "" || scope.IsLocal() {
		return
	}
	client, err := app.registryClient(session.Profile.Server)
	if err != nil {
		return
	}
	synced, err := runsync.Flush(app.context(), root, client, session.Token, scope.Organization, scope.Space)
	if err != nil {
		fmt.Fprintf(app.Stderr, "warning: run outbox flush: %v\n", err)
		return
	}
	if synced > 0 {
		fmt.Fprintf(app.Stderr, "synced %d local Run(s) to %s\n", synced, scope.String())
	}
}

func (app *App) checkRecipeDrift(recipe *manifest.RecipeDefinition) int {
	if recipe == nil {
		return ExitSuccess
	}
	store, err := app.configStore()
	if err != nil {
		return ExitSuccess
	}
	session, err := store.Session()
	if err != nil {
		if errors.Is(err, configstore.ErrNotLoggedIn) {
			return ExitSuccess
		}
		return ExitSuccess
	}
	scope := session.Profile.Context
	if scope.Organization == "" || scope.Space == "" || scope.IsLocal() {
		return ExitSuccess
	}
	client, err := app.registryClient(session.Profile.Server)
	if err != nil {
		fmt.Fprintf(app.Stderr, "warning: skip Recipe drift check: %v\n", err)
		return ExitSuccess
	}
	remote, err := client.Recipes(app.context(), session.Token, scope.Organization, scope.Space)
	if err != nil {
		fmt.Fprintf(app.Stderr, "warning: skip Recipe drift check: %v\n", err)
		return ExitSuccess
	}
	local := execution.ReferenceRecipe(*recipe)
	for _, item := range remote {
		if item.Name != local.Name || item.Revision.Version != local.Version {
			continue
		}
		if item.Revision.ManifestSHA256 == local.ManifestSHA256 &&
			item.Revision.Schema.ID == local.Schema.ID &&
			item.Revision.Schema.SHA256 == local.Schema.SHA256 {
			return ExitSuccess
		}
		fmt.Fprintf(app.Stderr, "Recipe %s@%s digest differs from Space %s\n", local.Name, local.Version, scope.String())
		fmt.Fprintln(app.Stderr, "Sync the Space working tree from Git, review the diff, then retry.")
		fmt.Fprintf(app.Stderr, "  local  %s\n", local.ManifestSHA256)
		fmt.Fprintf(app.Stderr, "  space  %s\n", item.Revision.ManifestSHA256)
		return ExitContract
	}
	return ExitSuccess
}
