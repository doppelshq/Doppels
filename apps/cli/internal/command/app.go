package command

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/projectlock"
	"doppels.so/cli/internal/version"
)

const (
	ExitSuccess     = 0
	ExitOperational = 1
	ExitContract    = 2
	ExitInterrupted = 130
)

type App struct {
	Context      context.Context
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
	Getwd        func() (string, error)
	Host         manifest.Host
	Environment  []string
	Hostname     func() (string, error)
	Now          func() time.Time
	ConfigDir    func() (string, error)
	HTTPClient   *http.Client
	Executable   func() (string, error)
	StartCommand func(*exec.Cmd) error
	Sleep        func(time.Duration)
}

func New() *App {
	return &App{
		Context: context.Background(), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getwd: os.Getwd, Host: manifest.OSHost{}, Environment: os.Environ(),
		Hostname: os.Hostname, Now: time.Now,
	}
}

func (app *App) Run(arguments []string) int {
	if len(arguments) == 0 {
		app.usage(app.Stdout)
		return ExitSuccess
	}
	switch arguments[0] {
	case "help", "-h", "--help":
		app.usage(app.Stdout)
		return ExitSuccess
	case "version", "-v", "--version":
		if app.experimentalEnabled() {
			fmt.Fprintf(app.Stdout, "doppels %s (experimental)\n", version.Version)
		} else {
			fmt.Fprintf(app.Stdout, "doppels %s\n", version.Version)
		}
		return ExitSuccess
	case "init":
		return app.runInit(arguments[1:])
	case "validate":
		return app.runValidate(arguments[1:])
	case "describe":
		return app.runDescribe(arguments[1:])
	case "run":
		return app.runLocal(arguments[1:])
	case "share":
		return app.runShare(arguments[1:])
	case "node":
		return app.runNode(arguments[1:])
	case "capabilities", "caps":
		return app.runCapabilities(arguments[1:])
	case "recipes":
		return app.runRecipes(arguments[1:])
	case "tree":
		return app.runTree(arguments[1:])
	case "runs":
		return app.runRuns(arguments[1:])
	case "organizations", "orgs":
		return app.runOrganizations(arguments[1:])
	case "org":
		return app.runOrg(arguments[1:])
	case "spaces":
		return app.runSpaces(arguments[1:])
	case "space":
		return app.runSpace(arguments[1:])
	case "login":
		return app.runLogin(arguments[1:])
	case "logout":
		return app.runLogout(arguments[1:])
	case "whoami":
		return app.runWhoAmI(arguments[1:])
	case "context":
		return app.runContext(arguments[1:])
	case "preview":
		return app.runPreview(arguments[1:])
	case "apply":
		return app.runApply(arguments[1:])
	case "publish":
		return app.runPublish(arguments[1:])
	case "unpublish":
		return app.runUnpublish(arguments[1:])
	case "install":
		return app.runInstall(arguments[1:])
	case "fork":
		return app.runFork(arguments[1:])
	case "prune":
		return app.runPrune(arguments[1:])
	case "telemetry":
		return app.runTelemetry(arguments[1:])
	case "update":
		return app.runUpdate(arguments[1:])
	case "experimental":
		return app.runExperimental(arguments[1:])
	default:
		fmt.Fprintf(app.Stderr, "unknown command %q\n\n", arguments[0])
		app.usage(app.Stderr)
		return ExitContract
	}
}

func (app *App) usage(writer io.Writer) {
	style := newTermStyle(writer)
	fmt.Fprintln(writer)
	header := style.bold("Doppels") + style.dim("  "+version.Version)
	if app.experimentalEnabled() {
		header += "  " + style.bold("[experimental]")
	}
	fmt.Fprintln(writer, header)
	fmt.Fprintln(writer, style.dim("  Local-first execution control plane"))
	fmt.Fprintln(writer)

	writeUsageSection(writer, style, "Space", []usageLine{
		{"doppels init [<name>]", "working tree + Space (default: private)"},
		{"doppels validate [-f manifest]", "check manifests"},
	})
	writeUsageSection(writer, style, "Run", []usageLine{
		{"doppels run [capability/<name>] [--yes] [--strict]", "execute locally · -d detach"},
		{"doppels describe (capability|recipe)/…", "inspect a definition"},
		{"doppels capabilities|caps [list]|show …", "list local Capabilities"},
		{"doppels recipes [list]|show …", "list local Recipes"},
		{"doppels tree [--json]", "Spaces → Capabilities → Recipes"},
		{"doppels runs [list]|show|logs …", "history (default 20)"},
	})
	writeUsageSection(writer, style, "Share", []usageLine{
		{"doppels share capability/<name>[@ver]", "create share link (--input · --yes)"},
	})
	writeUsageSection(writer, style, "Tooling", []usageLine{
		{"doppels update", "update to the latest release"},
		{"doppels telemetry accept|reject|status", "anonymous product analytics"},
		{"doppels experimental on|off|status", "enable preview features (cloud)"},
	})

	if app.experimentalEnabled() {
		fmt.Fprintln(writer, style.dim("Preview · Cloud"))
		writeUsageSubsection(writer, style, "Node", []usageLine{
			{"doppels node up", "this host online · approve / reject / skip inbox"},
		})
		writeUsageSubsection(writer, style, "Identity", []usageLine{
			{"doppels login|logout|whoami", "device login"},
			{"doppels organizations|orgs [list]", "cloud Orgs"},
			{"doppels org use · space use", "select Org / Space"},
			{"doppels context [show]", "current binding"},
		})
		writeUsageSubsection(writer, style, "Registry", []usageLine{
			{"doppels preview|apply [-f …]", "reconcile Space with cloud"},
			{"doppels publish capability/<name>", "list on Hub (--yes)"},
			{"doppels unpublish capability/<name>", "unlist from Hub"},
			{"doppels install @org/name[@ver]", "pin from Hub"},
			{"doppels fork @org/name", "copy into local tree"},
		})
	}

	fmt.Fprintln(writer, style.dim("Most commands accept --json.  doppels <cmd> --help for details."))
	fmt.Fprintln(writer, style.dim("Docs and community at doppels.so"))
	fmt.Fprintln(writer)
}

type usageLine struct {
	cmd  string
	hint string
}

func writeUsageSubsection(writer io.Writer, style termStyle, title string, lines []usageLine) {
	fmt.Fprintln(writer, "  "+style.dim("· "+title))
	const cmdWidth = 42
	for _, line := range lines {
		cmd := "    " + line.cmd
		pad := cmdWidth - visibleLen(cmd)
		if pad < 2 {
			pad = 2
		}
		fmt.Fprintf(writer, "%s%s%s\n", cmd, strings.Repeat(" ", pad), style.dim(line.hint))
	}
	fmt.Fprintln(writer)
}

func writeUsageSectionDimTitle(writer io.Writer, style termStyle, title string, lines []usageLine) {
	fmt.Fprintln(writer, style.dim(title))
	const cmdWidth = 42
	for _, line := range lines {
		cmd := "  " + line.cmd
		pad := cmdWidth - visibleLen(cmd)
		if pad < 2 {
			pad = 2
		}
		fmt.Fprintf(writer, "%s%s%s\n", cmd, strings.Repeat(" ", pad), style.dim(line.hint))
	}
	fmt.Fprintln(writer)
}

func writeUsageSection(writer io.Writer, style termStyle, title string, lines []usageLine) {
	fmt.Fprintln(writer, style.bold(title))
	const cmdWidth = 42
	for _, line := range lines {
		cmd := "  " + line.cmd
		pad := cmdWidth - visibleLen(cmd)
		if pad < 2 {
			pad = 2
		}
		fmt.Fprintf(writer, "%s%s%s\n", cmd, strings.Repeat(" ", pad), style.dim(line.hint))
	}
	fmt.Fprintln(writer)
}

func (app *App) flagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(app.Stderr)
	return flags
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func (app *App) runValidate(arguments []string) int {
	flags := app.flagSet("validate")
	var files stringList
	flags.Var(&files, "f", "manifest file to validate (repeatable)")
	flags.Var(&files, "file", "manifest file to validate (repeatable)")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "validate accepts manifest paths through -f")
		return ExitContract
	}
	root, paths, code := app.resolveInputs(files)
	if code != ExitSuccess {
		return code
	}
	documents, loadDiagnostics := load(paths)
	result := manifest.Validate(documents, manifest.ValidationOptions{Root: root, CheckHost: true, Host: app.Host})
	diagnostics := append(loadDiagnostics, result.Diagnostics...)
	manifest.SortDiagnostics(diagnostics)

	capabilities, recipes, spaces := definitionCounts(documents)
	lock, lockErr := projectlock.Load(root)
	if lockErr != nil {
		fmt.Fprintf(app.Stderr, "load %s: %v\n", projectlock.Filename, lockErr)
		return ExitOperational
	}
	items := buildValidateItems(root, documents, result.Catalog, lock)
	if *jsonOutput {
		status := "valid"
		if len(diagnostics) > 0 {
			status = "invalid"
		}
		app.writeJSON(map[string]any{
			"status":       status,
			"root":         root,
			"manifests":    len(documents),
			"capabilities": capabilities,
			"recipes":      recipes,
			"spaces":       spaces,
			"paths":        discoveryFolders(root, paths),
			"items":        items,
			"diagnostics":  diagnostics,
		})
	} else if len(diagnostics) == 0 {
		writeValidateReport(app.Stdout, root, paths, documents, items)
	} else {
		wroteHost := writeCatalogHostFailures(app.Stderr, result.Catalog, app.Host)
		for _, diagnostic := range diagnostics {
			if wroteHost && strings.HasPrefix(diagnostic.Code, "host.") {
				continue
			}
			fmt.Fprintln(app.Stderr, diagnostic.Error())
		}
		style := newTermStyle(app.Stderr)
		fmt.Fprintf(app.Stderr, "  %s  %s  %s\n", style.boldRed(crossMark(style)), style.bold("Validation failed"), style.dim(fmt.Sprintf("· %d error(s)", len(diagnostics))))
	}
	return diagnosticsExitCode(diagnostics)
}

func (app *App) runDescribe(arguments []string) int {
	flags := app.flagSet("describe")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(app.Stderr, "describe requires capability/<name>[@version] or recipe/<name>[@version]")
		return ExitContract
	}
	workingDirectory, err := app.Getwd()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
		return ExitOperational
	}
	root, err := project.FindRoot(workingDirectory)
	if err != nil {
		fmt.Fprintln(app.Stderr, missingLocalSpaceMessage())
		return ExitContract
	}
	paths, err := project.Discover(root)
	if err != nil {
		fmt.Fprintf(app.Stderr, "discover manifests: %v\n", err)
		return ExitOperational
	}
	documents, loadDiagnostics := load(paths)
	result := manifest.Validate(documents, manifest.ValidationOptions{Root: root, CheckHost: false})
	diagnostics := append(loadDiagnostics, result.Diagnostics...)
	manifest.SortDiagnostics(diagnostics)
	if len(diagnostics) > 0 {
		if *jsonOutput {
			app.writeJSON(map[string]any{"status": "invalid", "diagnostics": diagnostics})
		} else {
			for _, diagnostic := range diagnostics {
				fmt.Fprintln(app.Stderr, diagnostic.Error())
			}
		}
		return ExitContract
	}

	kind, reference, ok := strings.Cut(flags.Arg(0), "/")
	if !ok || reference == "" {
		fmt.Fprintln(app.Stderr, "resource must use kind/name syntax")
		return ExitContract
	}
	name, version, _ := strings.Cut(reference, "@")
	switch strings.ToLower(kind) {
	case "capability", "capabilities":
		definition, err := findCapability(result.Catalog, name, version)
		if err != nil {
			fmt.Fprintln(app.Stderr, err)
			return ExitContract
		}
		return app.describeCapability(definition, result.Catalog.RecipesForCapability(name), *jsonOutput)
	case "recipe", "recipes":
		definition, err := findRecipe(result.Catalog, name, version)
		if err != nil {
			fmt.Fprintln(app.Stderr, err)
			return ExitContract
		}
		return app.describeRecipe(definition, *jsonOutput)
	default:
		fmt.Fprintf(app.Stderr, "unsupported resource kind %q\n", kind)
		return ExitContract
	}
}

func (app *App) resolveInputs(explicit []string) (string, []string, int) {
	workingDirectory, err := app.Getwd()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
		return "", nil, ExitOperational
	}
	root, rootErr := project.FindRoot(workingDirectory)
	if rootErr != nil {
		if len(explicit) == 0 {
			fmt.Fprintln(app.Stderr, missingLocalSpaceMessage())
			return "", nil, ExitContract
		}
		root, err = filepath.Abs(workingDirectory)
		if err != nil {
			fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
			return "", nil, ExitOperational
		}
	}
	if len(explicit) == 0 {
		paths, err := project.Discover(root)
		if err != nil {
			fmt.Fprintf(app.Stderr, "discover manifests: %v\n", err)
			return "", nil, ExitOperational
		}
		return root, paths, ExitSuccess
	}
	paths := make([]string, 0, len(explicit))
	for _, path := range explicit {
		absolute, err := filepath.Abs(path)
		if err != nil {
			fmt.Fprintf(app.Stderr, "resolve manifest %s: %v\n", path, err)
			return "", nil, ExitOperational
		}
		paths = append(paths, absolute)
	}
	sort.Strings(paths)
	return root, paths, ExitSuccess
}

func load(paths []string) ([]manifest.Loaded, []manifest.Diagnostic) {
	documents := make([]manifest.Loaded, 0, len(paths))
	var diagnostics []manifest.Diagnostic
	for _, path := range paths {
		loaded, err := manifest.Load(path)
		if err != nil {
			diagnostics = append(diagnostics, manifest.Diagnostic{Source: path, Code: "manifest.decode", Message: err.Error()})
			continue
		}
		documents = append(documents, loaded)
	}
	return documents, diagnostics
}

func definitionCounts(documents []manifest.Loaded) (int, int, int) {
	capabilities, recipes, spaces := 0, 0, 0
	for _, document := range documents {
		switch document.Document.(type) {
		case *manifest.Capability:
			capabilities++
		case *manifest.Recipe:
			recipes++
		case *manifest.Space:
			spaces++
		}
	}
	return capabilities, recipes, spaces
}

func diagnosticsExitCode(diagnostics []manifest.Diagnostic) int {
	if len(diagnostics) == 0 {
		return ExitSuccess
	}
	for _, diagnostic := range diagnostics {
		if !strings.HasPrefix(diagnostic.Code, "host.") {
			return ExitContract
		}
	}
	return ExitOperational
}

// isTTY reports whether Stdout is an interactive terminal.
func (app *App) isTTY() bool {
	f, ok := app.Stdout.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func (app *App) writeJSON(value any) {
	encoder := json.NewEncoder(app.Stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func findCapability(catalog *manifest.Catalog, name, version string) (manifest.CapabilityDefinition, error) {
	matches := catalog.Capabilities[name]
	if version != "" {
		matches = filterCapabilities(matches, version)
	}
	if len(matches) == 0 {
		return manifest.CapabilityDefinition{}, fmt.Errorf("Capability %s not found", name)
	}
	if len(matches) > 1 {
		return manifest.CapabilityDefinition{}, fmt.Errorf("Capability %s has multiple revisions; include @version", name)
	}
	return matches[0], nil
}

func filterCapabilities(definitions []manifest.CapabilityDefinition, version string) []manifest.CapabilityDefinition {
	var matches []manifest.CapabilityDefinition
	for _, definition := range definitions {
		if definition.Value.Metadata.Version == version {
			matches = append(matches, definition)
		}
	}
	return matches
}

func findRecipe(catalog *manifest.Catalog, name, version string) (manifest.RecipeDefinition, error) {
	matches := catalog.Recipes[name]
	if version != "" {
		var filtered []manifest.RecipeDefinition
		for _, definition := range matches {
			if definition.Value.Metadata.Version == version {
				filtered = append(filtered, definition)
			}
		}
		matches = filtered
	}
	if len(matches) == 0 {
		return manifest.RecipeDefinition{}, fmt.Errorf("Recipe %s not found", name)
	}
	if len(matches) > 1 {
		return manifest.RecipeDefinition{}, fmt.Errorf("Recipe %s has multiple revisions; include @version", name)
	}
	return matches[0], nil
}

func (app *App) describeCapability(definition manifest.CapabilityDefinition, recipes []manifest.RecipeDefinition, jsonOutput bool) int {
	if jsonOutput {
		app.writeJSON(map[string]any{
			"kind":       "CapabilityDescription",
			"definition": definition.Value,
			"source":     sourceView(definition.Source),
			"recipes":    recipeViews(recipes),
		})
		return ExitSuccess
	}
	capability := definition.Value
	style := newTermStyle(app.Stdout)
	fmt.Fprintln(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s  %s@%s\n", style.field("Cap"), style.value(capability.Metadata.Name), capability.Metadata.Version)
	if capability.Metadata.DisplayName != "" {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Name"), capability.Metadata.DisplayName)
	}
	if capability.Metadata.Summary != "" {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Summary"), capability.Metadata.Summary)
	}
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Source"), style.dim(definition.Source.Path))
	writeContract(app.Stdout, style, "Inputs", capability.Inputs)
	writeContract(app.Stdout, style, "Outputs", capability.Outputs)
	fmt.Fprintln(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s\n", style.bold("Recipes"))
	if len(recipes) == 0 {
		fmt.Fprintln(app.Stdout, "    "+style.dim("none (human fulfillment remains available)"))
	} else {
		for _, recipe := range recipes {
			fmt.Fprintf(app.Stdout, "    %s@%s  %s\n", recipe.Value.Metadata.Name, recipe.Value.Metadata.Version, style.dim("("+recipe.Value.Runtime+")"))
		}
		if len(recipes) > 1 {
			fmt.Fprintln(app.Stdout, "    "+style.dim("explicit --recipe selection will be required to run"))
		}
	}
	return ExitSuccess
}

func (app *App) describeRecipe(definition manifest.RecipeDefinition, jsonOutput bool) int {
	if jsonOutput {
		app.writeJSON(map[string]any{"kind": "RecipeDescription", "definition": definition.Value, "source": sourceView(definition.Source)})
		return ExitSuccess
	}
	recipe := definition.Value
	style := newTermStyle(app.Stdout)
	fmt.Fprintln(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s  %s@%s\n", style.field("Recipe"), style.value(recipe.Metadata.Name), recipe.Metadata.Version)
	if recipe.Metadata.DisplayName != "" {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Name"), recipe.Metadata.DisplayName)
	}
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Runtime"), recipe.Runtime)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Provides"), strings.Join(recipe.Provides, ", "))
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Source"), style.dim(definition.Source.Path))
	if recipe.Runtime == "shell" {
		fmt.Fprintf(app.Stdout, "  %s  %d sequential\n", style.field("Steps"), len(recipe.Steps))
	}
	return ExitSuccess
}

func sourceView(source manifest.Loaded) map[string]string {
	return map[string]string{"path": source.Path, "directory": source.Directory, "sha256": source.SHA256}
}

func recipeViews(recipes []manifest.RecipeDefinition) []map[string]any {
	views := make([]map[string]any, 0, len(recipes))
	for _, recipe := range recipes {
		views = append(views, map[string]any{
			"name":    recipe.Value.Metadata.Name,
			"version": recipe.Value.Metadata.Version,
			"runtime": recipe.Value.Runtime,
			"source":  sourceView(recipe.Source),
		})
	}
	return views
}

func writeContract[T any](writer io.Writer, style termStyle, title string, values map[string]T) {
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s\n", style.bold(title))
	if len(values) == 0 {
		fmt.Fprintln(writer, "    "+style.dim("none"))
		return
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(writer, "    %s\n", style.cyan(name))
	}
}
