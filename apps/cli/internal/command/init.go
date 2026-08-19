package command

import (
	"bufio"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/project"
)

//go:embed initexamples
var initExamplesFS embed.FS

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

	// Offer demo examples only on default (no-arg) first init, TTY, no --json.
	var examplesSeeded bool
	if manifestCreated && flags.NArg() == 0 && !*jsonOutput && app.isTTY() {
		examplesSeeded = app.offerDemoExamples(root)
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
			"examplesSeeded":  examplesSeeded,
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
	if examplesSeeded {
		fmt.Fprintf(app.Stdout, "  %s\n", style.dim("Demo examples added. Try: doppels validate && doppels run capability/greet --input name=Ada --yes"))
	} else {
		fmt.Fprintf(app.Stdout, "  %s\n", style.dim("Next: add Capabilities/Recipes, then doppels validate && doppels run"))
	}
	return ExitSuccess
}

// offerDemoExamples asks the user whether to seed demo capabilities/recipes.
// Returns true if examples were written.
func (app *App) offerDemoExamples(root string) bool {
	style := newTermStyle(app.Stdout)
	fmt.Fprintln(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s\n", style.dim("Add demo examples (greet capability + recipe)? [y/N] "))

	reader := bufio.NewReader(app.Stdin)
	answer, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		return false
	}

	if err := seedDemoExamples(root); err != nil {
		fmt.Fprintf(app.Stderr, "seed demo examples: %v\n", err)
		return false
	}
	return true
}

// seedDemoExamples copies the embedded demo capability and recipe into
// .doppels/capabilities/ and .doppels/recipes/ under root.
func seedDemoExamples(root string) error {
	doppelsDir := filepath.Join(root, ".doppels")
	return fs.WalkDir(initExamplesFS, "initexamples", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("initexamples", path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dst := filepath.Join(doppelsDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := initExamplesFS.ReadFile(path)
		if err != nil {
			return err
		}
		// Do not overwrite existing files.
		if _, statErr := os.Stat(dst); statErr == nil {
			return nil
		}
		return os.WriteFile(dst, data, 0o644)
	})
}
