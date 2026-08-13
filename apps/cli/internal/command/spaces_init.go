package command

import (
	"fmt"
	"path/filepath"
	"strings"

	"doppels.so/cli/internal/project"
)

func (app *App) runSpacesInit(arguments []string) int {
	flags := app.flagSet("spaces init")
	dir := flags.String("dir", "", "directory for the Space working tree (default: cwd)")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() > 1 {
		fmt.Fprintln(app.Stderr, "spaces init accepts at most one Space name")
		return ExitContract
	}
	target := strings.TrimSpace(*dir)
	if target == "" {
		cwd, err := app.Getwd()
		if err != nil {
			fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
			return ExitOperational
		}
		target = cwd
	}
	spaceName := ""
	if flags.NArg() == 1 {
		spaceName = strings.TrimSpace(flags.Arg(0))
	}
	if spaceName == "" {
		base := filepath.Base(target)
		if base != "" && base != "." && base != string(filepath.Separator) {
			spaceName = base
		}
	}

	paths, err := project.Init(target)
	if err != nil {
		fmt.Fprintf(app.Stderr, "initialize Space working tree: %v\n", err)
		return ExitOperational
	}
	root, err := filepath.Abs(target)
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve Space root: %v\n", err)
		return ExitOperational
	}

	var manifestPath string
	manifestCreated := false
	if spaceName != "" {
		manifestPath, manifestCreated, err = project.WriteSpaceManifest(root, spaceName)
		if err != nil {
			fmt.Fprintf(app.Stderr, "write Space manifest: %v\n", err)
			return ExitContract
		}
	}

	if *jsonOutput {
		payload := map[string]any{
			"status":      "initialized",
			"root":        root,
			"directories": paths,
		}
		if spaceName != "" {
			payload["space"] = spaceName
		}
		if manifestPath != "" {
			payload["manifest"] = manifestPath
			payload["manifestCreated"] = manifestCreated
		}
		app.writeJSON(payload)
		return ExitSuccess
	}

	style := newTermStyle(app.Stdout)
	fmt.Fprintln(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Root"), style.value(root))
	if manifestPath != "" {
		rel := manifestPath
		if cwd, err := app.Getwd(); err == nil {
			if candidate, err := filepath.Rel(cwd, manifestPath); err == nil {
				rel = candidate
			}
		}
		if manifestCreated {
			fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Wrote"), rel)
		} else {
			fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Kept"), rel)
		}
	}
	if spaceName != "" {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Space"), spaceName)
	}
	fmt.Fprintln(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s\n", style.dim("Next: add Capabilities/Recipes, then doppels apply (or doppels login for cloud)"))
	return ExitSuccess
}

func missingLocalSpaceMessage() string {
	return "no local Space found; run doppels init"
}
