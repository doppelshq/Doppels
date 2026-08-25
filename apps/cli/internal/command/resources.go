package command

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	Space      string `json:"space,omitempty"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Summary    string `json:"summary,omitempty"`
	Pin        string `json:"pin,omitempty"`
	Recipes    int    `json:"recipes"`
	Runs       int    `json:"runs"`
	LastRun    string `json:"lastRun,omitempty"`
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
	index, code := app.listenLocalIndex()
	if code != ExitSuccess {
		return code
	}
	trees := index.localTrees()
	stats := app.loadCatalogRunIndex(index)
	items := capabilityListItems(trees, stats.capabilities)
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "CapabilityList", "items": items})
		return ExitSuccess
	}
	writeCapabilityList(app.Stdout, trees, items)
	return ExitSuccess
}

type recipeListItem struct {
	Space      string   `json:"space,omitempty"`
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Runtime    string   `json:"runtime"`
	Pin        string   `json:"pin,omitempty"`
	Provides   []string `json:"provides"`
	Runs       int      `json:"runs"`
	LastRun    string   `json:"lastRun,omitempty"`
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
	index, code := app.listenLocalIndex()
	if code != ExitSuccess {
		return code
	}
	trees := index.localTrees()
	stats := app.loadCatalogRunIndex(index)
	items := recipeListItems(trees, stats.recipes)
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "RecipeList", "items": items})
		return ExitSuccess
	}
	writeRecipeList(app.Stdout, trees, items)
	return ExitSuccess
}

type catalogRunStat struct {
	Count   int
	Last    string
	LastRaw string
}

type catalogRunIndex struct {
	capabilities map[string]catalogRunStat
	recipes      map[string]catalogRunStat
}

func (app *App) loadCatalogRunIndex(index listenLocalIndex) catalogRunIndex {
	stats := catalogRunIndex{
		capabilities: map[string]catalogRunStat{},
		recipes:      map[string]catalogRunStat{},
	}
	now := app.now()
	for _, entry := range index.Entries {
		items, err := runstate.List(entry.Root)
		if err != nil {
			continue
		}
		for _, item := range items {
			capName, capVersion := splitRevision(item.Capability)
			bumpCatalogRunStat(stats.capabilities, catalogListKey(entry.Space, capName, capVersion), item.CreatedAt, now)
			if strings.TrimSpace(item.Recipe) == "" {
				continue
			}
			recipeName, recipeVersion := splitRevision(item.Recipe)
			bumpCatalogRunStat(stats.recipes, catalogListKey(entry.Space, recipeName, recipeVersion), item.CreatedAt, now)
		}
	}
	return stats
}

func bumpCatalogRunStat(into map[string]catalogRunStat, key, createdAt string, now time.Time) {
	stat := into[key]
	stat.Count++
	if createdAt > stat.LastRaw {
		stat.LastRaw = createdAt
		stat.Last = formatDisplayTime(now, createdAt)
	}
	into[key] = stat
}

func capabilityListItems(trees []listenLocalTreeView, stats map[string]catalogRunStat) []capabilityListItem {
	items := make([]capabilityListItem, 0)
	for _, tree := range trees {
		for _, capability := range tree.Capabilities {
			name, version := splitRevision(capability.Label)
			stat := stats[catalogListKey(tree.Space, name, version)]
			items = append(items, capabilityListItem{
				Space:      tree.Space,
				Name:       name,
				Version:    version,
				Summary:    capability.Summary,
				Pin:        pinOrUnpinned(capability.Origin),
				Recipes:    len(capability.Recipes),
				Runs:       stat.Count,
				LastRun:    stat.Last,
				SourcePath: capability.Path,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		if items[i].Version != items[j].Version {
			return items[i].Version < items[j].Version
		}
		return items[i].Space < items[j].Space
	})
	return items
}

func recipeListItems(trees []listenLocalTreeView, stats map[string]catalogRunStat) []recipeListItem {
	items := make([]recipeListItem, 0)
	for _, tree := range trees {
		for _, capability := range tree.Capabilities {
			for _, recipe := range capability.Recipes {
				name, version := splitRevision(recipe.Label)
				stat := stats[catalogListKey(tree.Space, name, version)]
				items = append(items, recipeListItem{
					Space:      tree.Space,
					Name:       name,
					Version:    version,
					Runtime:    recipe.Runtime,
					Pin:        pinOrUnpinned(recipe.Origin),
					Provides:   append([]string(nil), recipe.Provides...),
					Runs:       stat.Count,
					LastRun:    stat.Last,
					SourcePath: recipe.Path,
				})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		if items[i].Version != items[j].Version {
			return items[i].Version < items[j].Version
		}
		return items[i].Space < items[j].Space
	})
	return items
}

func splitRevision(label string) (name, version string) {
	if i := strings.LastIndex(label, "@"); i > 0 {
		return label[:i], label[i+1:]
	}
	return label, ""
}

func writeCapabilityList(writer io.Writer, trees []listenLocalTreeView, items []capabilityListItem) {
	style := newTermStyle(writer)
	showSpace := catalogListShowSpace(trees)
	header := []string{style.dim("NAME"), style.dim("VERSION"), style.dim("PIN"), style.dim("HOST"), style.dim("RECIPES"), style.dim("RUNS"), style.dim("LAST"), style.dim("FILE")}
	if showSpace {
		header = append([]string{style.dim("SPACE")}, header...)
	}
	rows := [][]string{header}
	byKey := capabilityViewsByKey(trees)
	for _, item := range items {
		host := catalogHostMark(style, byKey[catalogListKey(item.Space, item.Name, item.Version)])
		last := item.LastRun
		if last == "" {
			last = "—"
		}
		row := []string{
			item.Name, item.Version, item.Pin, host, fmt.Sprintf("%d", item.Recipes),
			fmt.Sprintf("%d", item.Runs), last, catalogFileCell(style, trees, item.Space, item.SourcePath),
		}
		if showSpace {
			row = append([]string{item.Space}, row...)
		}
		rows = append(rows, row)
	}
	writeAlignedColumns(writer, rows)
}

func writeRecipeList(writer io.Writer, trees []listenLocalTreeView, items []recipeListItem) {
	style := newTermStyle(writer)
	showSpace := catalogListShowSpace(trees)
	header := []string{style.dim("NAME"), style.dim("VERSION"), style.dim("RUNTIME"), style.dim("PIN"), style.dim("HOST"), style.dim("PROVIDES"), style.dim("RUNS"), style.dim("LAST"), style.dim("FILE")}
	if showSpace {
		header = append([]string{style.dim("SPACE")}, header...)
	}
	rows := [][]string{header}
	byKey := recipeViewsByKey(trees)
	for _, item := range items {
		host := recipeHostMark(style, byKey[catalogListKey(item.Space, item.Name, item.Version)])
		last := item.LastRun
		if last == "" {
			last = "—"
		}
		row := []string{
			item.Name, item.Version, item.Runtime, item.Pin, host, strings.Join(item.Provides, ","),
			fmt.Sprintf("%d", item.Runs), last, catalogFileCell(style, trees, item.Space, item.SourcePath),
		}
		if showSpace {
			row = append([]string{item.Space}, row...)
		}
		rows = append(rows, row)
	}
	writeAlignedColumns(writer, rows)
}

func pinOrUnpinned(origin string) string {
	if strings.TrimSpace(origin) == "" {
		return "unpinned"
	}
	return origin
}

func catalogFileCell(style termStyle, trees []listenLocalTreeView, space, rel string) string {
	if strings.TrimSpace(rel) == "" {
		return "—"
	}
	abs := rel
	for _, tree := range trees {
		if tree.Space == space && tree.Path != "" {
			abs = filepath.Join(tree.Path, filepath.FromSlash(rel))
			break
		}
	}
	return style.fileLink(abs, filepath.ToSlash(rel))
}

func catalogListShowSpace(trees []listenLocalTreeView) bool {
	seen := map[string]struct{}{}
	for _, tree := range trees {
		seen[tree.Space] = struct{}{}
	}
	return len(seen) > 1
}

func catalogListKey(space, name, version string) string {
	return space + "\x00" + name + "@" + version
}

func capabilityViewsByKey(trees []listenLocalTreeView) map[string][]listenLocalRecipeView {
	out := map[string][]listenLocalRecipeView{}
	for _, tree := range trees {
		for _, capability := range tree.Capabilities {
			name, version := splitRevision(capability.Label)
			out[catalogListKey(tree.Space, name, version)] = capability.Recipes
		}
	}
	return out
}

func recipeViewsByKey(trees []listenLocalTreeView) map[string]listenLocalRecipeView {
	out := map[string]listenLocalRecipeView{}
	for _, tree := range trees {
		for _, capability := range tree.Capabilities {
			for _, recipe := range capability.Recipes {
				name, version := splitRevision(recipe.Label)
				out[catalogListKey(tree.Space, name, version)] = recipe
			}
		}
	}
	return out
}

func catalogHostMark(style termStyle, recipes []listenLocalRecipeView) string {
	if len(recipes) == 0 {
		return "—"
	}
	ready, checked := 0, 0
	for _, recipe := range recipes {
		if !recipe.Checked {
			continue
		}
		checked++
		if recipe.Ready {
			ready++
		}
	}
	if checked == 0 {
		return "—"
	}
	if ready == checked {
		return style.boldGreen("✓")
	}
	return style.boldRed("✗")
}

func recipeHostMark(style termStyle, recipe listenLocalRecipeView) string {
	if !recipe.Checked {
		return "—"
	}
	if recipe.Ready {
		return style.boldGreen("✓")
	}
	return style.boldRed("✗")
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
