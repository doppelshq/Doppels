package command

import (
	"errors"
	"fmt"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/registryclient"
)

func (app *App) runOrganizations(arguments []string) int {
	if isHelp(arguments) {
		fmt.Fprintln(app.Stdout, "Usage: doppels organizations|orgs [list] [--json]")
		return ExitSuccess
	}
	rest, ok := pluralDefaultList(arguments)
	if !ok {
		fmt.Fprintln(app.Stderr, "organizations requires list")
		return ExitContract
	}
	flags := app.flagSet("organizations list")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(rest); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "organizations list accepts no arguments")
		return ExitContract
	}
	session, code := app.loginSession()
	if code != ExitSuccess {
		return code
	}
	client, err := app.registryClient(session.Profile.Server)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitOperational
	}
	items, err := client.Organizations(app.context(), session.Token)
	if err != nil {
		fmt.Fprintf(app.Stderr, "list Organizations: %v\n", err)
		return ExitOperational
	}
	items = withLocalOrganization(items)
	if *jsonOutput {
		app.writeJSON(map[string]any{
			"kind":   "OrganizationList",
			"server": session.Profile.Server,
			"items":  items,
		})
		return ExitSuccess
	}
	style := newTermStyle(app.Stdout)
	if nonDefaultCloud(session.Profile.Server) {
		fmt.Fprintln(app.Stdout)
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Cloud"), session.Profile.Server)
		fmt.Fprintln(app.Stdout)
	}
	current := session.Profile.Context.Organization
	showDisplay := false
	for _, item := range items {
		if optionalString(item.DisplayName) != "" {
			showDisplay = true
			break
		}
	}
	header := []string{" ", style.dim("NAME")}
	if showDisplay {
		header = append(header, style.dim("DISPLAY"))
	}
	header = append(header, style.dim("UPDATED"))
	rows := [][]string{header}
	now := app.now()
	for _, item := range items {
		updated := formatDisplayTime(now, item.UpdatedAt)
		mark, name := selectionMark(style, item.Name, item.Name == current)
		row := []string{mark, name}
		if showDisplay {
			row = append(row, optionalString(item.DisplayName))
		}
		row = append(row, updated)
		rows = append(rows, row)
	}
	writeAlignedColumns(app.Stdout, rows)
	return ExitSuccess
}

func (app *App) runSpaces(arguments []string) int {
	if isHelp(arguments) {
		fmt.Fprintln(app.Stdout, "Usage: doppels spaces [list] [--json]")
		return ExitSuccess
	}
	if rest, ok := pluralDefaultList(arguments); ok {
		return app.runSpacesList(rest)
	}
	switch arguments[0] {
	case "init":
		fmt.Fprintln(app.Stderr, "use 'doppels init [<name>]' instead of 'doppels spaces init'")
		return ExitContract
	default:
		fmt.Fprintln(app.Stderr, "spaces requires list")
		return ExitContract
	}
}

func (app *App) runSpacesList(arguments []string) int {
	flags := app.flagSet("spaces list")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "spaces list accepts no arguments")
		return ExitContract
	}
	session, code := app.loginSession()
	if code != ExitSuccess {
		return code
	}
	organization := session.Profile.Context.Organization
	if organization == "" {
		fmt.Fprintln(app.Stderr, "no Organization selected; run doppels org use <organization>")
		return ExitContract
	}
	client, err := app.registryClient(session.Profile.Server)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitOperational
	}
	items, err := client.Spaces(app.context(), session.Token, organization)
	if err != nil {
		fmt.Fprintf(app.Stderr, "list Spaces: %v\n", err)
		return ExitOperational
	}
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "SpaceList", "organization": organization, "items": items})
		return ExitSuccess
	}
	style := newTermStyle(app.Stdout)
	showDisplay := false
	showSummary := false
	for _, item := range items {
		if optionalString(item.DisplayName) != "" {
			showDisplay = true
		}
		if optionalString(item.Summary) != "" {
			showSummary = true
		}
	}
	header := []string{" ", style.dim("NAME"), style.dim("AUTHORITY")}
	if showDisplay {
		header = append(header, style.dim("DISPLAY"))
	}
	if showSummary {
		header = append(header, style.dim("SUMMARY"))
	}
	header = append(header, style.dim("UPDATED"))
	rows := [][]string{header}
	now := app.now()
	currentSpace := session.Profile.Context.Space
	for _, item := range items {
		mark, name := selectionMark(style, item.Name, item.Name == currentSpace)
		row := []string{mark, name, item.SourceAuthority}
		if showDisplay {
			row = append(row, optionalString(item.DisplayName))
		}
		if showSummary {
			row = append(row, optionalString(item.Summary))
		}
		row = append(row, formatDisplayTime(now, item.UpdatedAt))
		rows = append(rows, row)
	}
	writeAlignedColumns(app.Stdout, rows)
	return ExitSuccess
}

func (app *App) loginSession() (configstore.Session, int) {
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return configstore.Session{}, ExitOperational
	}
	session, err := store.Session()
	if err == nil {
		return session, ExitSuccess
	}
	if errors.Is(err, configstore.ErrNotLoggedIn) {
		writeNotLoggedIn(app.Stderr)
		return configstore.Session{}, ExitContract
	}
	fmt.Fprintf(app.Stderr, "load login: %v\n", err)
	return configstore.Session{}, ExitOperational
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func selectionMark(style termStyle, name string, selected bool) (mark, displayName string) {
	if !selected {
		return " ", name
	}
	return style.boldCyan("*"), style.bold(name)
}

// withLocalOrganization prepends the offline local org so list always shows
// the selectable Context that org use local / init use.
func withLocalOrganization(items []registryclient.Organization) []registryclient.Organization {
	for _, item := range items {
		if item.Name == configstore.LocalOrganization {
			return items
		}
	}
	display := "Local"
	local := registryclient.Organization{
		ID:          configstore.LocalOrganization,
		Name:        configstore.LocalOrganization,
		DisplayName: &display,
	}
	return append([]registryclient.Organization{local}, items...)
}
