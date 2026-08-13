package command

import (
	"fmt"
	"sort"
	"strings"

	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/runstate"
)

func (app *App) runCapabilities(arguments []string) int {
	if isHelp(arguments) {
		fmt.Fprintln(app.Stdout, "Usage: doppels capabilities|caps [list] [--json]")
		fmt.Fprintln(app.Stdout, "       doppels capabilities|caps show <name>[@version] [--json]")
		return ExitSuccess
	}
	if rest, ok := pluralDefaultList(arguments); ok {
		return app.listCapabilities(rest)
	}
	switch arguments[0] {
	case "show":
		return app.showDefinition("capability", arguments[1:])
	default:
		fmt.Fprintf(app.Stderr, "unknown capabilities command %q\n", arguments[0])
		return ExitContract
	}
}

func (app *App) runRecipes(arguments []string) int {
	if isHelp(arguments) {
		fmt.Fprintln(app.Stdout, "Usage: doppels recipes [list] [--json]")
		fmt.Fprintln(app.Stdout, "       doppels recipes show <name>[@version] [--json]")
		return ExitSuccess
	}
	if rest, ok := pluralDefaultList(arguments); ok {
		return app.listRecipes(rest)
	}
	switch arguments[0] {
	case "show":
		return app.showDefinition("recipe", arguments[1:])
	default:
		fmt.Fprintf(app.Stderr, "unknown recipes command %q\n", arguments[0])
		return ExitContract
	}
}

func (app *App) showDefinition(kind string, arguments []string) int {
	// Keep `describe kind/name` as the compatibility path and a single source
	// of rendering/JSON semantics.
	if len(arguments) == 0 {
		fmt.Fprintf(app.Stderr, "%ss show requires <name>[@version]\n", kind)
		return ExitContract
	}
	resourceIndex := 0
	if arguments[0] == "--json" {
		resourceIndex = 1
	}
	if resourceIndex >= len(arguments) {
		fmt.Fprintf(app.Stderr, "%ss show requires <name>[@version]\n", kind)
		return ExitContract
	}
	forwarded := append([]string(nil), arguments...)
	if !strings.Contains(forwarded[resourceIndex], "/") {
		forwarded[resourceIndex] = kind + "/" + forwarded[resourceIndex]
	}
	return app.runDescribe(resourceFirst(forwarded))
}

type capabilityListItem struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Summary    string `json:"summary,omitempty"`
	Recipes    int    `json:"recipes"`
	SourcePath string `json:"source"`
}

func (app *App) listCapabilities(arguments []string) int {
	flags := app.flagSet("capabilities list")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "capabilities list accepts no arguments")
		return ExitContract
	}
	_, catalog, code := app.localCatalog()
	if code != ExitSuccess {
		return code
	}
	items := make([]capabilityListItem, 0)
	for _, revisions := range catalog.Capabilities {
		for _, definition := range revisions {
			items = append(items, capabilityListItem{
				Name: definition.Value.Metadata.Name, Version: definition.Value.Metadata.Version,
				Summary:    definition.Value.Metadata.Summary,
				Recipes:    len(catalog.RecipesForCapability(definition.Value.Metadata.Name)),
				SourcePath: definition.Source.Path,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Version < items[j].Version
	})
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "CapabilityList", "items": items})
		return ExitSuccess
	}
	style := newTermStyle(app.Stdout)
	showSummary := false
	for _, item := range items {
		if strings.TrimSpace(item.Summary) != "" {
			showSummary = true
			break
		}
	}
	header := []string{style.dim("NAME"), style.dim("VERSION"), style.dim("RECIPES")}
	if showSummary {
		header = append(header, style.dim("SUMMARY"))
	}
	rows := [][]string{header}
	for _, item := range items {
		row := []string{item.Name, item.Version, fmt.Sprintf("%d", item.Recipes)}
		if showSummary {
			row = append(row, item.Summary)
		}
		rows = append(rows, row)
	}
	writeAlignedColumns(app.Stdout, rows)
	return ExitSuccess
}

type recipeListItem struct {
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Runtime    string   `json:"runtime"`
	Provides   []string `json:"provides"`
	SourcePath string   `json:"source"`
}

func (app *App) listRecipes(arguments []string) int {
	flags := app.flagSet("recipes list")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "recipes list accepts no arguments")
		return ExitContract
	}
	_, catalog, code := app.localCatalog()
	if code != ExitSuccess {
		return code
	}
	items := make([]recipeListItem, 0)
	for _, revisions := range catalog.Recipes {
		for _, definition := range revisions {
			items = append(items, recipeListItem{
				Name: definition.Value.Metadata.Name, Version: definition.Value.Metadata.Version,
				Runtime: definition.Value.Runtime, Provides: append([]string(nil), definition.Value.Provides...),
				SourcePath: definition.Source.Path,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Version < items[j].Version
	})
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "RecipeList", "items": items})
		return ExitSuccess
	}
	style := newTermStyle(app.Stdout)
	rows := [][]string{{
		style.dim("NAME"), style.dim("VERSION"), style.dim("RUNTIME"), style.dim("PROVIDES"),
	}}
	for _, item := range items {
		rows = append(rows, []string{
			item.Name, item.Version, item.Runtime, strings.Join(item.Provides, ","),
		})
	}
	writeAlignedColumns(app.Stdout, rows)
	return ExitSuccess
}

func (app *App) runRuns(arguments []string) int {
	if isHelp(arguments) {
		fmt.Fprintln(app.Stdout, "Usage: doppels runs [list] [--limit N|--all] [--json]")
		fmt.Fprintln(app.Stdout, "       doppels runs show <run-id> [--json]")
		fmt.Fprintln(app.Stdout, "       doppels runs logs <run-id> [--follow|-f] [--json]")
		return ExitSuccess
	}
	if rest, ok := pluralDefaultList(arguments); ok {
		return app.listRuns(rest)
	}
	switch arguments[0] {
	case "show":
		return app.showRun(arguments[1:])
	case "logs":
		return app.showRunLogs(arguments[1:])
	default:
		fmt.Fprintf(app.Stderr, "unknown runs command %q\n", arguments[0])
		return ExitContract
	}
}

// pluralDefaultList treats bare plural commands (and leading flags) as list.
// doppels capabilities  ·  doppels capabilities --json  ·  doppels capabilities list …
func pluralDefaultList(arguments []string) (rest []string, ok bool) {
	if len(arguments) == 0 {
		return nil, true
	}
	switch {
	case arguments[0] == "list":
		return arguments[1:], true
	case strings.HasPrefix(arguments[0], "-"):
		return arguments, true
	default:
		return arguments, false
	}
}

func isHelp(arguments []string) bool {
	return len(arguments) == 1 && (arguments[0] == "help" || arguments[0] == "-h" || arguments[0] == "--help")
}

func (app *App) localRoot() (string, int) {
	workingDirectory, err := app.Getwd()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
		return "", ExitOperational
	}
	root, err := project.FindRoot(workingDirectory)
	if err != nil {
		fmt.Fprintln(app.Stderr, missingLocalSpaceMessage())
		return "", ExitContract
	}
	return root, ExitSuccess
}

func (app *App) listRuns(arguments []string) int {
	flags := app.flagSet("runs list")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	limit := flags.Int("limit", defaultRunListLimit, "max Runs to show in human output")
	all := flags.Bool("all", false, "show every Run in human output")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "runs list accepts no arguments")
		return ExitContract
	}
	if !*all && *limit < 1 {
		fmt.Fprintln(app.Stderr, "--limit must be >= 1 (or use --all)")
		return ExitContract
	}
	root, code := app.localRoot()
	if code != ExitSuccess {
		return code
	}
	items, err := runstate.List(root)
	if err != nil {
		fmt.Fprintf(app.Stderr, "list local Runs: %v\n", err)
		return ExitOperational
	}
	app.maybeFlushOutbox()
	items = app.mergeCloudRuns(items)
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "RunList", "items": items})
		return ExitSuccess
	}
	total := len(items)
	shown := items
	if !*all && total > *limit {
		shown = items[:*limit]
	}
	now := app.now()
	writeRunList(app.Stdout, shown, total, *limit, *all, func(created string) string {
		return formatDisplayTime(now, created)
	})
	return ExitSuccess
}

// mergeCloudRuns unions local summaries with Space Runs from the control plane
// when a login session and Org/Space Context exist. Cloud outages keep local
// rows; duplicate ids keep cloud metadata with source=cloud.
func (app *App) mergeCloudRuns(local []runstate.Summary) []runstate.Summary {
	store, err := app.configStore()
	if err != nil {
		return local
	}
	session, err := store.Session()
	if err != nil {
		return local
	}
	scope := session.Profile.Context
	if scope.Organization == "" || scope.Space == "" || scope.IsLocal() {
		return local
	}
	client, err := app.registryClient(session.Profile.Server)
	if err != nil {
		fmt.Fprintf(app.Stderr, "warning: skip Cloud Runs: %v\n", err)
		return local
	}
	remote, err := client.ListRuns(app.context(), session.Token, scope.Organization, scope.Space)
	if err != nil {
		fmt.Fprintf(app.Stderr, "warning: skip Cloud Runs: %v\n", err)
		return local
	}
	return mergeRunSummaries(local, remote)
}

func mergeRunSummaries(local []runstate.Summary, remote []registryclient.SpaceRun) []runstate.Summary {
	byID := make(map[string]runstate.Summary, len(local)+len(remote))
	order := make([]string, 0, len(local)+len(remote))
	for _, item := range local {
		byID[item.ID] = item
		order = append(order, item.ID)
	}
	for _, item := range remote {
		summary := runstate.Summary{
			ID: item.ID, Status: item.Status, Source: item.Source,
			CreatedAt: item.CreatedAt, RequestID: item.RequestID,
			Capability: item.Capability, Recipe: item.Recipe,
		}
		if summary.Source == "" {
			summary.Source = "cloud"
		}
		if existing, ok := byID[summary.ID]; ok {
			// Prefer cloud label when the same Run is known locally and remotely.
			summary.StateDir = existing.StateDir
			summary.SyncStatus = existing.SyncStatus
			byID[summary.ID] = summary
			continue
		}
		byID[summary.ID] = summary
		order = append(order, summary.ID)
	}
	result := make([]runstate.Summary, 0, len(order))
	for _, id := range order {
		result = append(result, byID[id])
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].CreatedAt > result[j].CreatedAt
	})
	return result
}

func (app *App) showRun(arguments []string) int {
	flags := app.flagSet("runs show")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(app.Stderr, "runs show requires <run-id>")
		return ExitContract
	}
	root, code := app.localRoot()
	if code != ExitSuccess {
		return code
	}
	runID, err := resolveRunID(root, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(app.Stderr, "show local Run: %v\n", err)
		return ExitContract
	}
	detail, err := runstate.Load(root, runID)
	if err != nil {
		fmt.Fprintf(app.Stderr, "show local Run: %v\n", err)
		return ExitOperational
	}
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "RunDescription", "run": detail})
		return ExitSuccess
	}
	writeRunShow(app.Stdout, detail, app.now())
	return ExitSuccess
}

func (app *App) showRunLogs(arguments []string) int {
	flags := app.flagSet("runs logs")
	follow := false
	flags.BoolVar(&follow, "follow", false, "follow step logs until the Run finishes")
	flags.BoolVar(&follow, "f", false, "follow step logs until the Run finishes")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(app.Stderr, "runs logs requires <run-id>")
		return ExitContract
	}
	if follow && *jsonOutput {
		fmt.Fprintln(app.Stderr, "--follow cannot be combined with --json")
		return ExitContract
	}
	root, code := app.localRoot()
	if code != ExitSuccess {
		return code
	}
	runID, err := resolveRunID(root, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(app.Stderr, "read local Run: %v\n", err)
		return ExitContract
	}
	if follow {
		return app.followRunLogs(root, runID)
	}
	detail, err := runstate.Load(root, runID)
	if err != nil {
		fmt.Fprintf(app.Stderr, "read local Run: %v\n", err)
		return ExitOperational
	}
	logs, err := runstate.Logs(root, runID)
	if err != nil {
		fmt.Fprintf(app.Stderr, "read local Run logs: %v\n", err)
		return ExitOperational
	}
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "RunLogs", "items": logs})
		return ExitSuccess
	}
	writeRunLogs(app.Stdout, detail, logs)
	return ExitSuccess
}

func trimRunResource(value string) string {
	for _, prefix := range []string{"run/", "runs/"} {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return value
}
