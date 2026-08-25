package command

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/projectlock"
	"doppels.so/cli/internal/registryclient"
)

type listenFilters struct {
	Organization string
	Space        string
	Capability   string
}

type listenCapabilityView struct {
	Organization   string
	Space          string
	Name           string
	Version        string
	Label          string
	HasRecipe      bool
	RecipeName     string
	Mode           string // "recipe" or "manual"
	CapabilityPath string
	RecipePath     string
	Checked        bool
	Ready          bool
	Missing        []string
}

type listenScopeView struct {
	Header            listenHeader
	Organization      string
	Scopes            []registryclient.ListenScope
	Capabilities      []listenCapabilityView
	LocalCapabilities []string
	LocalProjects     []string
	LocalTrees        []listenLocalTreeView
	Inbox             []listenInboxItemView
}

type listenLocalCapabilityView struct {
	Label   string
	Path    string
	Origin  string
	Summary string
	Recipes []listenLocalRecipeView
}

type listenLocalRecipeView struct {
	Label    string
	Path     string
	Origin   string
	Runtime  string
	Provides []string
	Checked  bool
	Ready    bool
	Missing  []string
}

type listenLocalTreeView struct {
	Path         string
	Space        string
	Branch       string
	Worktree     string
	Capabilities []listenLocalCapabilityView
}

type listenInboxItemView struct {
	ID         string
	Capability string
	From       string
	Age        string
}

type listenPromptQueue struct {
	Index  int
	Total  int
	Queued []string
}

type fulfillDecision int

const (
	fulfillApprove fulfillDecision = iota
	fulfillReject
	fulfillSkip
	fulfillBackground
)

func (app *App) resolveListenScope(server, apiToken string, store *configstore.Store, catalog *manifest.Catalog, filters listenFilters) (listenScopeView, listenFilters, *registryclient.Client, error) {
	view := listenScopeView{Header: app.listenHeader(server, apiToken, store)}
	filters, err := resolveListenFilters(store, view.Header, filters)
	if err != nil {
		return view, filters, nil, err
	}
	view.Organization = filters.Organization
	view.Header.Organization = filters.Organization
	if filters.Space != "" {
		view.Header.Space = filters.Space
	} else {
		view.Header.Space = ""
	}

	registry, err := app.registryClient(server)
	if err != nil {
		return view, filters, nil, err
	}
	inbox, err := registry.ListenInbox(app.context(), apiToken, registryclient.ListenFilters{
		Organization: filters.Organization,
		Space:        filters.Space,
		Capability:   filters.Capability,
	})
	if err != nil {
		return view, filters, registry, err
	}
	view.Scopes = inbox.Scopes
	view.LocalCapabilities = listLocalCapabilityNames(catalog)
	view.Capabilities = buildListenCapabilities(inbox.Scopes, catalog, app.Host)
	view.Inbox = buildListenInbox(inbox, app.now())
	return view, filters, registry, nil
}

func buildListenInbox(inbox *registryclient.ListenInbox, now time.Time) []listenInboxItemView {
	if inbox == nil {
		return nil
	}
	items := make([]listenInboxItemView, 0, len(inbox.Requests)+len(inbox.Shares))
	for _, pending := range inbox.Requests {
		if pending.Status != "requested" && pending.Status != "approved" {
			continue
		}
		cap := pending.Summary.Capability.Name
		if pending.Summary.Capability.Version != "" {
			cap += "@" + pending.Summary.Capability.Version
		}
		items = append(items, listenInboxItemView{
			ID:         pending.Summary.ID,
			Capability: cap,
			From:       listenRequester(pending.Summary.RequestedBy),
			Age:        formatRelativeTime(now, pending.Summary.CreatedAt),
		})
	}
	for _, share := range inbox.Shares {
		if !share.AwaitingFulfillment {
			continue
		}
		cap := share.Share.CapabilityRevision.Name
		if share.Share.CapabilityRevision.Version != "" {
			cap += "@" + share.Share.CapabilityRevision.Version
		}
		items = append(items, listenInboxItemView{
			ID:         share.Share.ID,
			Capability: cap,
			From:       listenRequester(share.Share.SharedBy),
			Age:        "share",
		})
	}
	return items
}

func listLocalCapabilityNames(catalog *manifest.Catalog) []string {
	views := listLocalCapabilityViews(catalog, "", nil, nil)
	names := make([]string, 0, len(views))
	for _, view := range views {
		names = append(names, view.Label)
	}
	return names
}

func listLocalCapabilityViews(catalog *manifest.Catalog, root string, host manifest.Host, lock *projectlock.File) []listenLocalCapabilityView {
	if catalog == nil {
		return nil
	}
	names := make([]string, 0, len(catalog.Capabilities))
	for name := range catalog.Capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	views := make([]listenLocalCapabilityView, 0, len(names))
	for _, name := range names {
		definitions := catalog.Capabilities[name]
		if len(definitions) == 0 || definitions[0].Value == nil {
			continue
		}
		label := name
		if definitions[0].Value.Metadata.Version != "" {
			label = name + "@" + definitions[0].Value.Metadata.Version
		}
		view := listenLocalCapabilityView{
			Label:   label,
			Path:    listenRelPath(root, definitions[0].Source.Path),
			Origin:  resourceOrigin(lock, "Capability", name, definitions[0].Value.Metadata.Version, definitions[0].Source.SHA256),
			Summary: definitions[0].Value.Metadata.Summary,
		}
		for _, recipe := range catalog.RecipesForCapability(name) {
			if recipe.Value == nil {
				continue
			}
			recipeLabel := recipe.Value.Metadata.Name
			if recipe.Value.Metadata.Version != "" {
				recipeLabel += "@" + recipe.Value.Metadata.Version
			}
			item := listenLocalRecipeView{
				Label:    recipeLabel,
				Path:     listenRelPath(root, recipe.Source.Path),
				Origin:   resourceOrigin(lock, "Recipe", recipe.Value.Metadata.Name, recipe.Value.Metadata.Version, recipe.Source.SHA256),
				Runtime:  recipe.Value.Runtime,
				Provides: append([]string(nil), recipe.Value.Provides...),
			}
			if host != nil {
				item.Checked = true
				item.Missing = manifest.CheckRequires(recipe.Value, catalog.Root, host)
				item.Ready = len(item.Missing) == 0
			}
			view.Recipes = append(view.Recipes, item)
		}
		views = append(views, view)
	}
	return views
}

func listenRelPath(root, path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	clean := filepath.Clean(path)
	if strings.TrimSpace(root) == "" {
		return filepath.ToSlash(clean)
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil || strings.HasPrefix(rel, "..") {
		absRoot, errRoot := filepath.Abs(root)
		absPath, errPath := filepath.Abs(clean)
		if errRoot == nil && errPath == nil {
			if rel, err = filepath.Rel(absRoot, absPath); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
		return filepath.ToSlash(clean)
	}
	return filepath.ToSlash(rel)
}

func resolveListenFilters(store *configstore.Store, header listenHeader, filters listenFilters) (listenFilters, error) {
	if filters.Organization == "" {
		filters.Organization = strings.TrimSpace(header.Organization)
	}
	if filters.Organization == "" && store != nil {
		if current, err := store.Context(); err == nil {
			filters.Organization = strings.TrimSpace(current.Organization)
		}
	}
	if filters.Organization == "" {
		return filters, fmt.Errorf("no Organization selected; run doppels org use <organization> or pass --org")
	}
	if filters.Space != "" && filters.Organization == "" {
		return filters, fmt.Errorf("--space requires --org or a selected context")
	}
	return filters, nil
}

func buildListenCapabilities(scopes []registryclient.ListenScope, catalog *manifest.Catalog, host manifest.Host) []listenCapabilityView {
	catalogRoot := ""
	local := map[string]manifest.CapabilityDefinition{}
	if catalog != nil {
		catalogRoot = catalog.Root
		for _, definitions := range catalog.Capabilities {
			for _, definition := range definitions {
				local[definition.Value.Metadata.Name] = definition
			}
		}
	}
	seen := map[string]bool{}
	views := make([]listenCapabilityView, 0)
	for _, scope := range scopes {
		for _, capability := range scope.Capabilities {
			key := scope.Organization + "/" + scope.Space + "/" + capability.Name + "@" + capability.Version
			if seen[key] {
				continue
			}
			seen[key] = true
			localDef, ok := local[capability.Name]
			if !ok {
				continue
			}
			if localDef.Value.Metadata.Version != "" && capability.Version != "" &&
				localDef.Value.Metadata.Version != capability.Version {
				continue
			}
			label := capability.Name + "@" + capability.Version
			if capability.DisplayName != nil && strings.TrimSpace(*capability.DisplayName) != "" {
				label = strings.TrimSpace(*capability.DisplayName) + " (" + label + ")"
			}
			view := listenCapabilityView{
				Organization:   scope.Organization,
				Space:          scope.Space,
				Name:           capability.Name,
				Version:        capability.Version,
				Label:          label,
				Mode:           "manual",
				CapabilityPath: listenRelPath(catalogRoot, localDef.Source.Path),
			}
			if catalog != nil {
				recipes := catalog.RecipesForCapability(capability.Name)
				if len(recipes) > 0 {
					view.HasRecipe = true
					view.Mode = "recipe"
					view.RecipeName = recipes[0].Value.Metadata.Name
					if recipes[0].Value.Metadata.Version != "" {
						view.RecipeName += "@" + recipes[0].Value.Metadata.Version
					}
					view.RecipePath = listenRelPath(catalogRoot, recipes[0].Source.Path)
					if host != nil {
						view.Checked = true
						view.Missing = manifest.CheckRequires(recipes[0].Value, catalog.Root, host)
						view.Ready = len(view.Missing) == 0
					}
				}
			}
			views = append(views, view)
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Space != views[j].Space {
			return views[i].Space < views[j].Space
		}
		if views[i].HasRecipe != views[j].HasRecipe {
			return views[i].HasRecipe
		}
		return views[i].Name < views[j].Name
	})
	return views
}

// Deprecated alias kept for older tests during rename.
func intersectListenCapabilities(scopes []registryclient.ListenScope, catalog *manifest.Catalog) []listenCapabilityView {
	return buildListenCapabilities(scopes, catalog, nil)
}

func writeListenBanner(writer io.Writer, view listenScopeView) {
	style := newTermStyle(writer)
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.boldCyan("●")+" "+style.bold("Node online"))
	fmt.Fprintln(writer)
	writeListenField(writer, style, "Node", valueOrDash(view.Header.Node))
	writeListenField(writer, style, "Identity", valueOrDash(view.Header.Identity))
	writeListenField(writer, style, "Scope", valueOrDash(view.Organization))
	writeListenField(writer, style, "Server", valueOrDash(view.Header.Server))
	fmt.Fprintln(writer)
	localOnly := strings.TrimSpace(view.Organization) == configstore.LocalOrganization
	if len(view.Capabilities) > 0 {
		fmt.Fprintln(writer, "  "+style.bold("Applied"))
		writeListenTree(writer, style, "  ", catalogTreeItems(view))
	} else if !localOnly {
		fmt.Fprintln(writer, "  "+style.bold("Applied"))
		if len(view.Scopes) == 0 {
			fmt.Fprintln(writer, "    "+style.dim("None in this Organization."))
		} else {
			fmt.Fprintln(writer, "    "+style.dim("None of these local Capabilities are applied here."))
		}
	}
	writeListenLocalTrees(writer, style, view.LocalTrees)
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.bold("Inbox"))
	if len(view.Inbox) == 0 {
		fmt.Fprintln(writer, "    "+style.dim("0 pending"))
	} else {
		fmt.Fprintf(writer, "    %s\n", style.dim(listenCount(len(view.Inbox), "pending", "pending")))
		for _, item := range view.Inbox {
			fmt.Fprintf(writer, "    %s  %s  %s  %s\n",
				style.label(shortListenID(item.ID)),
				style.value(item.Capability),
				style.dim(item.From),
				style.dim(item.Age),
			)
		}
	}
	writeListenBannerStatus(writer, style, view)
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.dim("[a]pprove  [r]eject  [s]kip  [b]ackground   ·  Ctrl-C stops this Node"))
	fmt.Fprintln(writer)
}

type listenTreeItem struct {
	Label    string
	Mark     string
	Hint     string
	Suffix   string
	Dim      bool
	Children []listenTreeItem
}

func listenDisplayName(label string) string {
	if i := strings.LastIndex(label, "@"); i > 0 {
		return label[:i]
	}
	return label
}

func revisionLabel(name, version string) string {
	name = strings.TrimSpace(name)
	version = strings.TrimSpace(version)
	if name == "" {
		return version
	}
	if version == "" {
		return name
	}
	return name + "@" + version
}

func writeListenTree(writer io.Writer, style termStyle, indent string, items []listenTreeItem) {
	for i, item := range items {
		last := i == len(items)-1
		tee, childIndent := "├── ", indent+"│   "
		if last {
			tee, childIndent = "└── ", indent+"    "
		}
		mark := ""
		switch item.Mark {
		case "ok":
			mark = style.boldGreen("✓") + " "
		case "fail":
			mark = style.boldRed("✗") + " "
		case "warn":
			mark = style.boldYellow("!") + " "
		}
		name := item.Label
		if item.Dim {
			name = style.dim(item.Label)
		} else {
			name = style.value(item.Label)
		}
		line := indent + tee + mark + name
		if item.Hint != "" {
			line += "  " + style.dim(item.Hint)
		}
		if item.Suffix != "" {
			line += "  " + style.dim(item.Suffix)
		}
		fmt.Fprintln(writer, line)
		if len(item.Children) > 0 {
			writeListenTree(writer, style, childIndent, item.Children)
		}
	}
}

func recipeRequireMarks(checked, ready bool, missing []string) string {
	if !checked {
		return ""
	}
	if ready {
		return "ok"
	}
	return "fail"
}

func catalogPinMark(origin, hostMark string) string {
	if hostMark == "fail" {
		return "fail"
	}
	if origin == "stale" {
		return "warn"
	}
	return hostMark
}

func catalogTreeHint(parts ...string) string {
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, "  ")
}

func listenRecipeTreeItem(label, path, origin, runtime string, checked, ready bool, missing []string) listenTreeItem {
	item := listenTreeItem{
		Label:  label,
		Mark:   catalogPinMark(origin, recipeRequireMarks(checked, ready, missing)),
		Hint:   catalogTreeHint(runtime, pinOrUnpinned(origin)),
		Suffix: path,
	}
	for _, need := range missing {
		item.Children = append(item.Children, listenTreeItem{Label: need, Dim: true})
	}
	return item
}

func catalogTreeItems(view listenScopeView) []listenTreeItem {
	display := map[string]string{}
	for _, scope := range view.Scopes {
		label := scope.Space
		if scope.DisplayName != nil && strings.TrimSpace(*scope.DisplayName) != "" {
			label += " — " + strings.TrimSpace(*scope.DisplayName)
		}
		display[scope.Space] = label
	}
	var order []string
	grouped := map[string][]listenCapabilityView{}
	for _, capability := range view.Capabilities {
		if _, ok := grouped[capability.Space]; !ok {
			order = append(order, capability.Space)
		}
		grouped[capability.Space] = append(grouped[capability.Space], capability)
	}
	items := make([]listenTreeItem, 0, len(order))
	for _, space := range order {
		label := space
		if pretty, ok := display[space]; ok {
			label = pretty
		}
		item := listenTreeItem{Label: label}
		for _, capability := range grouped[space] {
			child := listenTreeItem{
				Label:  capability.Label,
				Hint:   catalogTreeHint(catalogTreeModeHint(capability.HasRecipe && capability.RecipeName != ""), "applied"),
				Suffix: capability.CapabilityPath,
			}
			if capability.HasRecipe && capability.RecipeName != "" {
				child.Children = append(child.Children, listenRecipeTreeItem(
					capability.RecipeName, capability.RecipePath, "applied", "",
					capability.Checked, capability.Ready, capability.Missing,
				))
			}
			item.Children = append(item.Children, child)
		}
		items = append(items, item)
	}
	return items
}

func writeListenLocalTrees(writer io.Writer, style termStyle, trees []listenLocalTreeView) {
	if len(trees) == 0 {
		return
	}
	fmt.Fprintln(writer)
	heading := "  " + style.bold("Local")
	if root := listenLocalRootLabel(trees); root != "" {
		heading += "  " + style.dim(root)
	}
	fmt.Fprintln(writer, heading)
	items := make([]listenTreeItem, 0, len(trees))
	for _, tree := range trees {
		item := listenTreeItem{
			Label:  valueOrDash(tree.Space),
			Suffix: listenLocalTreeExtra(tree),
		}
		for _, capability := range tree.Capabilities {
			child := listenTreeItem{
				Label:  capability.Label,
				Mark:   catalogPinMark(capability.Origin, ""),
				Hint:   catalogTreeHint(catalogTreeModeHint(len(capability.Recipes) > 0), pinOrUnpinned(capability.Origin)),
				Suffix: capability.Path,
			}
			for _, recipe := range capability.Recipes {
				child.Children = append(child.Children, listenRecipeTreeItem(
					recipe.Label, recipe.Path, recipe.Origin, recipe.Runtime, recipe.Checked, recipe.Ready, recipe.Missing,
				))
			}
			item.Children = append(item.Children, child)
		}
		items = append(items, item)
	}
	writeListenTree(writer, style, "  ", items)
}

func listenLocalRootLabel(trees []listenLocalTreeView) string {
	if len(trees) < 2 {
		return ""
	}
	parent := filepath.Dir(trees[0].Path)
	if parent == "" || parent == "." || parent == string(filepath.Separator) {
		return ""
	}
	for _, tree := range trees[1:] {
		if filepath.Dir(tree.Path) != parent {
			return ""
		}
	}
	return filepath.Base(parent)
}

func listenLocalTreeStats(tree listenLocalTreeView) (caps, recipes, blocked int) {
	caps = len(tree.Capabilities)
	for _, capability := range tree.Capabilities {
		recipes += len(capability.Recipes)
		for _, recipe := range capability.Recipes {
			if recipe.Checked && !recipe.Ready {
				blocked++
			}
		}
	}
	return caps, recipes, blocked
}

func listenLocalTreeCounts(tree listenLocalTreeView) string {
	caps, recipes, blocked := listenLocalTreeStats(tree)
	parts := []string{
		listenCount(caps, "cap", "caps"),
		listenCount(recipes, "recipe", "recipes"),
	}
	if blocked > 0 {
		parts = append(parts, listenCount(blocked, "blocked", "blocked"))
	}
	return strings.Join(parts, " · ")
}

func catalogTreeMode(hasRecipe bool) string {
	if hasRecipe {
		return "recipe"
	}
	return "manual"
}

func catalogTreeModeHint(hasRecipe bool) string {
	if hasRecipe {
		return ""
	}
	return "manual"
}

func listenLocalTreeExtra(tree listenLocalTreeView) string {
	var parts []string
	if counts := listenLocalTreeCounts(tree); counts != "" {
		parts = append(parts, counts)
	}
	if branch := strings.TrimSpace(tree.Branch); branch != "" && branch != "—" && branch != "main" && branch != "master" {
		parts = append(parts, branch)
	}
	if worktree := strings.TrimSpace(tree.Worktree); worktree != "" && worktree != "—" && worktree != "primary" {
		parts = append(parts, worktree)
	}
	return strings.Join(parts, " · ")
}

type listenBlockedRecipe struct {
	Space   string
	Label   string
	Missing []string
}

func listenLocalRecipeStats(view listenScopeView) (ready, blocked int, blockedRecipes []listenBlockedRecipe) {
	for _, tree := range view.LocalTrees {
		for _, capability := range tree.Capabilities {
			for _, recipe := range capability.Recipes {
				if !recipe.Checked {
					continue
				}
				if recipe.Ready {
					ready++
					continue
				}
				blocked++
				blockedRecipes = append(blockedRecipes, listenBlockedRecipe{
					Space:   tree.Space,
					Label:   recipe.Label,
					Missing: append([]string(nil), recipe.Missing...),
				})
			}
		}
	}
	return ready, blocked, blockedRecipes
}

func writeListenBannerStatus(writer io.Writer, style termStyle, view listenScopeView) {
	ready, blocked, blockedRecipes := listenLocalRecipeStats(view)
	catalogWarn, catalogDetail := listenCatalogWarn(view)
	if ready == 0 && blocked == 0 && catalogWarn == "" {
		return
	}
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.bold("Status"))
	if blocked+ready > 0 {
		var parts []string
		if blocked > 0 {
			parts = append(parts, style.boldRed("✗")+" "+listenCount(blocked, "Recipe not ready", "Recipes not ready"))
		}
		if ready > 0 {
			parts = append(parts, style.boldGreen("✓")+" "+listenCount(ready, "Recipe ready", "Recipes ready"))
		}
		fmt.Fprintln(writer, "    "+strings.Join(parts, "  ·  "))
	}
	for _, recipe := range blockedRecipes {
		line := "    " + style.boldRed("✗") + " " + style.value(valueOrDash(recipe.Space)+"/"+recipe.Label)
		if len(recipe.Missing) > 0 {
			line += "  " + style.dim(strings.Join(recipe.Missing, " · "))
		}
		fmt.Fprintln(writer, line)
	}
	if catalogWarn != "" {
		fmt.Fprintln(writer, "    "+style.boldYellow("!")+" "+catalogWarn)
		if catalogDetail != "" {
			fmt.Fprintln(writer, "      "+style.dim(catalogDetail))
		}
	}
}

func listenCatalogWarn(view listenScopeView) (title, detail string) {
	if strings.TrimSpace(view.Organization) == configstore.LocalOrganization {
		return "", ""
	}
	if len(view.Capabilities) > 0 {
		return "", ""
	}
	hasLocalCaps := len(view.LocalCapabilities) > 0
	if !hasLocalCaps {
		for _, tree := range view.LocalTrees {
			if len(tree.Capabilities) > 0 {
				hasLocalCaps = true
				break
			}
		}
	}
	if hasLocalCaps {
		return "None of these local Capabilities are applied here.", "Apply these Spaces into " + valueOrDash(view.Organization) + ", or switch org."
	}
	if len(view.LocalTrees) > 0 || len(view.LocalProjects) > 0 {
		return "No Capability manifests under .doppels/capabilities (or discovery: in doppels.<space>.yaml).", ""
	}
	if len(view.Scopes) == 0 {
		return "None in this Organization.", ""
	}
	return "", ""
}

func resourceOrigin(lock *projectlock.File, kind, name, version, digest string) string {
	if lock == nil {
		return "unpinned"
	}
	for _, entry := range lock.Resources {
		if entry.Kind != kind || entry.Revision.Name != name || entry.Revision.Version != version {
			continue
		}
		if digest != "" && entry.Revision.ManifestSHA256 != digest {
			return "stale"
		}
		return "pinned"
	}
	return "unpinned"
}

type lockPinIssue struct {
	Kind    string
	Name    string
	Version string
	Path    string
	Root    string
}

func collectLockPinIssues(root string, lock *projectlock.File, capability manifest.CapabilityDefinition, recipe *manifest.RecipeDefinition) []lockPinIssue {
	var issues []lockPinIssue
	if resourceOrigin(lock, "Capability", capability.Value.Metadata.Name, capability.Value.Metadata.Version, capability.Source.SHA256) == "stale" {
		issues = append(issues, lockPinIssue{
			Kind: "Capability", Name: capability.Value.Metadata.Name, Version: capability.Value.Metadata.Version,
			Path: capability.Source.Path, Root: root,
		})
	}
	if recipe != nil && resourceOrigin(lock, "Recipe", recipe.Value.Metadata.Name, recipe.Value.Metadata.Version, recipe.Source.SHA256) == "stale" {
		issues = append(issues, lockPinIssue{
			Kind: "Recipe", Name: recipe.Value.Metadata.Name, Version: recipe.Value.Metadata.Version,
			Path: recipe.Source.Path, Root: root,
		})
	}
	return issues
}

func (app *App) checkLockPin(root string, capability manifest.CapabilityDefinition, recipe *manifest.RecipeDefinition, strict bool) ([]lockPinIssue, int) {
	lock, err := projectlock.Load(root)
	if err != nil {
		fmt.Fprintf(app.Stderr, "load %s: %v\n", projectlock.Filename, err)
		if strict {
			return nil, ExitContract
		}
		return nil, ExitSuccess
	}
	issues := collectLockPinIssues(root, lock, capability, recipe)
	writeLockPinCard(app.Stderr, issues, strict)
	if len(issues) == 0 || !strict {
		return issues, ExitSuccess
	}
	return issues, ExitContract
}

func writeListenField(writer io.Writer, style termStyle, label, value string) {
	fmt.Fprintf(writer, "  %s  %s\n", style.label(fmt.Sprintf("%-8s", label)), style.value(value))
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func writeListenScopeStatus(writer io.Writer, scopeCount, capabilityCount, shareOpen, shareReady, spaceReady int) {
	style := newTermStyle(writer)
	var msg string
	switch {
	case shareReady+spaceReady > 0:
		msg = style.boldYellow("● " + listenCount(shareReady+spaceReady, "Request ready", "Requests ready") + " to decide")
	case shareOpen > 0:
		msg = style.cyan("● " + fmt.Sprintf("%s · %d Space(s) · %d Capabilit(ies)",
			listenCount(shareOpen, "Share", "Shares"), scopeCount, capabilityCount))
	default:
		parts := []string{}
		if scopeCount > 0 {
			parts = append(parts, fmt.Sprintf("%d Space(s)", scopeCount))
		}
		if capabilityCount > 0 {
			parts = append(parts, fmt.Sprintf("%d Capabilit(ies)", capabilityCount))
		}
		if len(parts) == 0 {
			msg = style.dim("● waiting for a run…")
		} else {
			msg = style.dim("● " + strings.Join(parts, " · ") + " · waiting for a run…")
		}
	}
	fmt.Fprintf(writer, "\r  %s", padStatusLine(msg, 64))
}

func writeListenStatus(writer io.Writer, open, idle, ready int) {
	_ = idle
	writeListenScopeStatus(writer, 0, 0, open, ready, 0)
}
