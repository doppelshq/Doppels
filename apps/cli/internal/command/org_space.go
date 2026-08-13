package command

import (
	"errors"
	"fmt"
	"strings"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/project"
)

func (app *App) runOrg(arguments []string) int {
	if isHelp(arguments) {
		fmt.Fprintln(app.Stdout, "Usage: doppels org use <organization> [--json]")
		return ExitSuccess
	}
	if len(arguments) == 0 || arguments[0] != "use" {
		fmt.Fprintln(app.Stderr, "org requires use; for listing: doppels organizations list")
		return ExitContract
	}
	return app.orgUse(arguments[1:])
}

func (app *App) orgUse(arguments []string) int {
	flags := app.flagSet("org use")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(app.Stderr, "org use requires <organization>")
		return ExitContract
	}
	organization := strings.TrimSpace(flags.Arg(0))
	selected := configstore.Context{Organization: organization}
	if !selected.Valid() {
		fmt.Fprintln(app.Stderr, "organization name must be a lowercase identifier")
		return ExitContract
	}
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return ExitOperational
	}
	current, currentErr := store.Context()
	space := ""
	if currentErr == nil && current.Organization == organization {
		space = current.Space
	} else if organization == configstore.LocalOrganization {
		space = configstore.LocalSpace
	}
	selected.Space = space
	if err := store.SetContext(selected); err != nil {
		if errors.Is(err, configstore.ErrNotLoggedIn) {
			fmt.Fprintln(app.Stderr, err)
			return ExitContract
		}
		fmt.Fprintf(app.Stderr, "save context: %v\n", err)
		return ExitOperational
	}
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "OrganizationSelected", "context": selected})
	} else {
		style := newTermStyle(app.Stdout)
		if selected.Space == "" {
			fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Org"), style.value(selected.Organization))
			fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Next"), style.dim("doppels space use <space>"))
		} else {
			fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Context"), style.value(selected.String()))
		}
	}
	return ExitSuccess
}

func (app *App) runSpace(arguments []string) int {
	if isHelp(arguments) {
		fmt.Fprintln(app.Stdout, "Usage: doppels space use <space> [--json]")
		return ExitSuccess
	}
	if len(arguments) == 0 || arguments[0] != "use" {
		fmt.Fprintln(app.Stderr, "space requires use")
		return ExitContract
	}
	return app.spaceUse(arguments[1:])
}

func (app *App) spaceUse(arguments []string) int {
	flags := app.flagSet("space use")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(app.Stderr, "space use requires <space>")
		return ExitContract
	}
	space := strings.TrimSpace(flags.Arg(0))
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return ExitOperational
	}
	current, err := store.Context()
	if err != nil {
		fmt.Fprintln(app.Stderr, "no Organization selected; run doppels org use <organization>")
		return ExitContract
	}
	selected := configstore.Context{Organization: current.Organization, Space: space}
	if !selected.Valid() || selected.Space == "" {
		fmt.Fprintln(app.Stderr, "space name must be a lowercase identifier")
		return ExitContract
	}
	if err := store.SetContext(selected); err != nil {
		if errors.Is(err, configstore.ErrNotLoggedIn) {
			fmt.Fprintln(app.Stderr, err)
			return ExitContract
		}
		fmt.Fprintf(app.Stderr, "save context: %v\n", err)
		return ExitOperational
	}
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "SpaceSelected", "context": selected})
	} else {
		style := newTermStyle(app.Stdout)
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Context"), style.value(selected.String()))
		app.warnMissingSpaceManifest(selected.Space)
	}
	return ExitSuccess
}

func (app *App) warnMissingSpaceManifest(space string) {
	if app.Getwd == nil {
		return
	}
	cwd, err := app.Getwd()
	if err != nil {
		return
	}
	root, err := project.FindRoot(cwd)
	if err != nil {
		return
	}
	_, found, err := project.FindSpaceManifest(root, space)
	if err != nil || found {
		return
	}
	style := newTermStyle(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Note"), style.dim(fmt.Sprintf(
		"no doppels.%s.yaml here — create with doppels spaces init %s before cloud apply",
		space, space,
	)))
}
