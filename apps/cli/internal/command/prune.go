package command

import (
	"fmt"

	"doppels.so/cli/internal/registryclient"
)

// runPrune unregisters manifest-managed Space registrations absent from the
// local Space working tree. It never touches cloud-managed registrations, Organization-
// level Capability/Recipe/Revision rows or the Space itself.
func (app *App) runPrune(arguments []string) int {
	flags := app.flagSet("prune")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	yes := flags.Bool("yes", false, "execute the unregister plan (default is a dry run)")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "prune accepts no positional arguments; the keep set is discovered from the local Space")
		return ExitContract
	}
	input, code := app.reconcileInput("")
	if code != ExitSuccess {
		return code
	}
	keep := make([]registryclient.KeepEntry, 0, len(input.body.Resources))
	for _, resource := range input.body.Resources {
		keep = append(keep, registryclient.KeepEntry{Kind: resource.Kind, Name: resource.Revision.Name})
	}
	client, err := app.registryClient(input.session.Profile.Server)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitOperational
	}
	server := input.session.Profile.Server
	if !*yes {
		return app.runPrunePlan(client, input, keep, server, *jsonOutput)
	}
	return app.runPruneApply(client, input, keep, server, *jsonOutput)
}

func (app *App) runPrunePlan(client *registryclient.Client, input reconcileInput, keep []registryclient.KeepEntry, server string, jsonOutput bool) int {
	response, err := client.PrunePlan(app.context(), input.session.Token, input.scope.Organization, input.scope.Space, keep)
	if err != nil {
		fmt.Fprintf(app.Stderr, "prune: %v\n", err)
		return reconcileErrorCode(err)
	}
	if jsonOutput {
		app.writeJSON(map[string]any{
			"kind": "Prune", "server": server, "context": input.scope, "changes": response.Changes,
			"applicable": response.Applicable, "pruned": false,
		})
		return ExitSuccess
	}
	writeChanges(app.Stdout, server, input.scope, response.Changes, nil)
	style := newTermStyle(app.Stdout)
	if len(response.Changes) == 0 {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Prune"), style.dim("Nothing to prune"))
	} else {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Prune"), style.dim("Dry run only; re-run with --yes to unregister"))
	}
	return ExitSuccess
}

func (app *App) runPruneApply(client *registryclient.Client, input reconcileInput, keep []registryclient.KeepEntry, server string, jsonOutput bool) int {
	response, err := client.Prune(app.context(), input.session.Token, input.scope.Organization, input.scope.Space, keep)
	if err != nil {
		fmt.Fprintf(app.Stderr, "prune: %v\n", err)
		return reconcileErrorCode(err)
	}
	if !response.Pruned {
		fmt.Fprintln(app.Stderr, "Cloud did not confirm the prune")
		return ExitOperational
	}
	if jsonOutput {
		app.writeJSON(map[string]any{
			"kind": "Prune", "server": server, "context": input.scope, "changes": response.Changes,
			"pruned": true,
		})
		return ExitSuccess
	}
	writeChanges(app.Stdout, server, input.scope, response.Changes, nil)
	style := newTermStyle(app.Stdout)
	if len(response.Changes) == 0 {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Prune"), style.dim("Nothing to prune"))
	} else {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Prune"), style.boldGreen(checkMark(style))+" "+style.bold("Pruned"))
	}
	return ExitSuccess
}
