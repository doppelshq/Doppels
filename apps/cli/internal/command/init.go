package command

import (
	"fmt"
	"path/filepath"
	"strings"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/project"
)

func (app *App) runInit(arguments []string) int {
	flags := app.flagSet("init")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() > 1 {
		fmt.Fprintln(app.Stderr, "init accepts at most one Space name (e.g. doppels init myspace)")
		return ExitContract
	}
	target, err := app.Getwd()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
		return ExitOperational
	}

	spaceName := configstore.LocalSpace
	if flags.NArg() == 1 {
		spaceName = strings.TrimSpace(flags.Arg(0))
	}

	root, err := filepath.Abs(target)
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve Space root: %v\n", err)
		return ExitOperational
	}
	paths, err := project.Init(root)
	if err != nil {
		fmt.Fprintf(app.Stderr, "initialize Space working tree: %v\n", err)
		return ExitOperational
	}
	manifestPath, manifestCreated, err := project.WriteSpaceManifest(root, spaceName)
	if err != nil {
		fmt.Fprintf(app.Stderr, "write Space manifest: %v\n", err)
		return ExitContract
	}

	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return ExitOperational
	}
	if err := store.SetContext(configstore.LocalContext()); err != nil {
		fmt.Fprintf(app.Stderr, "set local context: %v\n", err)
		return ExitOperational
	}
	if err := store.SetBinding(configstore.LocalOrganization, spaceName, root); err != nil {
		fmt.Fprintf(app.Stderr, "record Space binding: %v\n", err)
		return ExitOperational
	}

	if *jsonOutput {
		app.writeJSON(map[string]any{
			"status":          "initialized",
			"root":            root,
			"space":           spaceName,
			"context":         configstore.LocalContext(),
			"manifest":        manifestPath,
			"manifestCreated": manifestCreated,
			"directories":     paths,
		})
		return ExitSuccess
	}
	style := newTermStyle(app.Stdout)
	fmt.Fprintln(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Root"), style.value(root))
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Space"), spaceName)
	if manifestCreated {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Wrote"), filepath.Base(manifestPath))
	} else {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Kept"), filepath.Base(manifestPath))
	}
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Context"), configstore.LocalContext().String())
	fmt.Fprintln(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s\n", style.dim("Next: add Capabilities/Recipes, then doppels validate && doppels run"))
	return ExitSuccess
}
