package command

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/registryclient"
)

type listenFilters struct {
	Organization string
	Space        string
	Capability   string
}

type listenCapabilityView struct {
	Organization string
	Space        string
	Name         string
	Version      string
	Label        string
	HasRecipe    bool
	RecipeName   string
	Mode         string // "recipe" or "manual"
}

type listenScopeView struct {
	Header            listenHeader
	Organization      string
	Scopes            []registryclient.ListenScope
	Capabilities      []listenCapabilityView
	LocalCapabilities []string
	LocalProjects     []string
}

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
	view.Capabilities = buildListenCapabilities(inbox.Scopes, catalog)
	return view, filters, registry, nil
}

func listLocalCapabilityNames(catalog *manifest.Catalog) []string {
	if catalog == nil {
		return nil
	}
	names := make([]string, 0, len(catalog.Capabilities))
	for name, definitions := range catalog.Capabilities {
		label := name
		if len(definitions) > 0 && definitions[0].Value != nil && definitions[0].Value.Metadata.Version != "" {
			label = name + "@" + definitions[0].Value.Metadata.Version
		}
		names = append(names, label)
	}
	sort.Strings(names)
	return names
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

func buildListenCapabilities(scopes []registryclient.ListenScope, catalog *manifest.Catalog) []listenCapabilityView {
	local := map[string]manifest.CapabilityDefinition{}
	if catalog != nil {
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
				Organization: scope.Organization,
				Space:        scope.Space,
				Name:         capability.Name,
				Version:      capability.Version,
				Label:        label,
				Mode:         "manual",
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
	return buildListenCapabilities(scopes, catalog)
}

func writeListenBanner(writer io.Writer, view listenScopeView) {
	style := newTermStyle(writer)
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.boldCyan("●")+" "+style.bold("Listening"))
	fmt.Fprintln(writer)
	writeListenField(writer, style, "Node", valueOrDash(view.Header.Node))
	writeListenField(writer, style, "Server", valueOrDash(view.Header.Server))
	writeListenField(writer, style, "Identity", valueOrDash(view.Header.Identity))
	writeListenField(writer, style, "Org", valueOrDash(view.Organization))
	fmt.Fprintln(writer)
	if len(view.Scopes) == 0 {
		fmt.Fprintln(writer, "  "+style.dim("No accessible Spaces in this Organization."))
	} else {
		fmt.Fprintln(writer, "  "+style.bold("Spaces"))
		for _, scope := range view.Scopes {
			label := scope.Space
			if scope.DisplayName != nil && strings.TrimSpace(*scope.DisplayName) != "" {
				label += " — " + strings.TrimSpace(*scope.DisplayName)
			}
			fmt.Fprintln(writer, "    "+style.value(label))
		}
	}
	fmt.Fprintln(writer)
	if len(view.Capabilities) == 0 {
		fmt.Fprintln(writer, "  "+style.bold("Ready to fulfill"))
		fmt.Fprintln(writer, "  "+style.dim("None — local Capabilities are not registered in these Spaces."))
		if len(view.LocalProjects) > 1 {
			fmt.Fprintln(writer, "  "+style.dim("Local Spaces: "+strings.Join(view.LocalProjects, ", ")))
		}
		if len(view.LocalCapabilities) > 0 {
			fmt.Fprintln(writer, "  "+style.dim("Local: "+strings.Join(view.LocalCapabilities, ", ")))
			fmt.Fprintln(writer, "  "+style.dim("Listen only lists Capabilities present both here and in Org Spaces."))
			fmt.Fprintln(writer, "  "+style.dim("Switch context to the Space you applied, or apply this Space into "+valueOrDash(view.Organization)+"."))
		} else {
			fmt.Fprintln(writer, "  "+style.dim("No Capability manifests found under discovered local Spaces."))
		}
	} else {
		fmt.Fprintln(writer, "  "+style.bold("Ready to fulfill"))
		spaceWidth := 0
		nameWidth := 0
		for _, capability := range view.Capabilities {
			if len(capability.Space) > spaceWidth {
				spaceWidth = len(capability.Space)
			}
			if len(capability.Label) > nameWidth {
				nameWidth = len(capability.Label)
			}
		}
		if spaceWidth > 24 {
			spaceWidth = 24
		}
		if nameWidth > 44 {
			nameWidth = 44
		}
		for _, capability := range view.Capabilities {
			space := padListenLabel(capability.Space, spaceWidth)
			padded := padListenLabel(capability.Label, nameWidth)
			mode := style.dim("manual")
			detail := "no Recipe"
			if capability.HasRecipe {
				mode = style.boldGreen("recipe")
				detail = capability.RecipeName
			}
			fmt.Fprintf(writer, "    %s  %s  %s  %s\n",
				style.dim(space),
				style.value(padded),
				mode,
				style.dim(detail),
			)
		}
	}
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "  "+style.dim("Waiting for Requests in "+valueOrDash(view.Organization)+"."))
	fmt.Fprintln(writer, "  "+style.dim("When a Request arrives, you can approve or reject it."))
	fmt.Fprintln(writer, "  "+style.dim("Press Ctrl-C to quit."))
	fmt.Fprintln(writer)
}

func padListenLabel(label string, width int) string {
	if len(label) >= width {
		return label
	}
	return label + strings.Repeat(" ", width-len(label))
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
