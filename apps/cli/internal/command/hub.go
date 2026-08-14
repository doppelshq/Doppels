package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/registryclient"
)

func (app *App) runPublish(arguments []string) int {
	flags := app.flagSet("publish")
	recipeName := flags.String("recipe", "", "compatible Recipe name[@version]")
	yes := flags.Bool("yes", false, "upload Capability and Recipe source without prompting")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(app.Stderr, "publish requires capability/<name>[@version]")
		return ExitContract
	}
	_, catalog, code := app.localCatalog()
	if code != ExitSuccess {
		return code
	}
	capabilityDefinition, err := resolveCapabilityArgument(catalog, flags.Arg(0))
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	recipeDefinition, err := catalog.ResolveRecipe(capabilityDefinition.Value.Metadata.Name, *recipeName)
	if err != nil {
		interaction := newInteraction(app.Stdin, app.Stderr)
		if errors.Is(err, manifest.ErrRecipeAmbiguous) && *recipeName == "" && !*jsonOutput && interaction.canPrompt() {
			picked, pickErr := interaction.pickRecipe(catalog.RecipesForCapability(capabilityDefinition.Value.Metadata.Name))
			if pickErr != nil {
				fmt.Fprintln(app.Stderr, pickErr)
				return sharePromptExitCode(pickErr)
			}
			recipeDefinition = picked
		} else {
			fmt.Fprintln(app.Stderr, err)
			if errors.Is(err, manifest.ErrRecipeAmbiguous) && *recipeName == "" {
				fmt.Fprintln(app.Stderr, "hint: pass --recipe <name>[@version]")
			}
			return ExitContract
		}
	}
	organization, session, code := app.cloudOrganization()
	if code != ExitSuccess {
		return code
	}
	capabilitySource, err := os.ReadFile(capabilityDefinition.Source.Path)
	if err != nil {
		fmt.Fprintf(app.Stderr, "read Capability source: %v\n", err)
		return ExitOperational
	}
	recipeSource, err := os.ReadFile(recipeDefinition.Source.Path)
	if err != nil {
		fmt.Fprintf(app.Stderr, "read Recipe source: %v\n", err)
		return ExitOperational
	}
	ref := "@" + organization + "/" + capabilityDefinition.Value.Metadata.Name
	if !*yes {
		fmt.Fprintf(app.Stderr, "publish will upload %s and Recipe %s@%s to the public hub\n", ref, recipeDefinition.Value.Metadata.Name, recipeDefinition.Value.Metadata.Version)
		fmt.Fprintln(app.Stderr, "pass --yes to publish")
		return ExitContract
	}
	client, err := app.registryClient(session.Profile.Server)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitOperational
	}
	listing, err := client.Publish(app.context(), session.Token, organization, registryclient.PublishRequest{
		CapabilityName:    capabilityDefinition.Value.Metadata.Name,
		CapabilityVersion: capabilityDefinition.Value.Metadata.Version,
		CapabilitySummary: capabilityDefinition.Value.Metadata.Summary,
		CapabilitySource:  string(capabilitySource),
		RecipeName:        recipeDefinition.Value.Metadata.Name,
		RecipeVersion:     recipeDefinition.Value.Metadata.Version,
		RecipeSource:      string(recipeSource),
	})
	if err != nil {
		fmt.Fprintf(app.Stderr, "publish: %v\n", err)
		return ExitOperational
	}
	return app.writeHubResult("Publish", listing, *jsonOutput)
}

func (app *App) runUnpublish(arguments []string) int {
	flags := app.flagSet("unpublish")
	yes := flags.Bool("yes", false, "unpublish without prompting")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(app.Stderr, "unpublish requires capability/<name>")
		return ExitContract
	}
	kind, reference, ok := strings.Cut(flags.Arg(0), "/")
	if !ok || reference == "" || (kind != "capability" && kind != "capabilities") {
		fmt.Fprintln(app.Stderr, "resource must use capability/<name>")
		return ExitContract
	}
	name, _, _ := strings.Cut(reference, "@")
	organization, session, code := app.cloudOrganization()
	if code != ExitSuccess {
		return code
	}
	if !*yes {
		fmt.Fprintf(app.Stderr, "unpublish will unlist @%s/%s; the URL stays\n", organization, name)
		fmt.Fprintln(app.Stderr, "pass --yes to unpublish")
		return ExitContract
	}
	client, err := app.registryClient(session.Profile.Server)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitOperational
	}
	listing, err := client.Unpublish(app.context(), session.Token, organization, name)
	if err != nil {
		fmt.Fprintf(app.Stderr, "unpublish: %v\n", err)
		return ExitOperational
	}
	return app.writeHubResult("Unpublish", listing, *jsonOutput)
}

func (app *App) runInstall(arguments []string) int {
	flags := app.flagSet("install")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	serverFlag := flags.String("server", "", "Doppels control-plane URL")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(app.Stderr, "install requires @org/name[@version]")
		return ExitContract
	}
	org, name, version, err := parseHubRef(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	root, listing, code := app.fetchHubListing(org, name, *serverFlag)
	if code != ExitSuccess {
		return code
	}
	if version != "" && listing.CapabilityVersion != version {
		fmt.Fprintf(app.Stderr, "hub listing is %s@%s, not @%s\n", listing.CapabilityName, listing.CapabilityVersion, version)
		return ExitContract
	}
	moduleDir := filepath.Join(root, project.Directory, "modules", "@"+org, name)
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		fmt.Fprintf(app.Stderr, "create module directory: %v\n", err)
		return ExitOperational
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "capability.yaml"), []byte(listing.CapabilitySource), 0o600); err != nil {
		fmt.Fprintf(app.Stderr, "write Capability: %v\n", err)
		return ExitOperational
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "recipe.yaml"), []byte(listing.RecipeSource), 0o600); err != nil {
		fmt.Fprintf(app.Stderr, "write Recipe: %v\n", err)
		return ExitOperational
	}
	pin, err := json.MarshalIndent(map[string]any{
		"organization":      listing.Organization,
		"capabilityName":    listing.CapabilityName,
		"capabilityVersion": listing.CapabilityVersion,
		"recipeName":        listing.RecipeName,
		"recipeVersion":     listing.RecipeVersion,
		"status":            listing.Status,
		"publicPath":        listing.PublicPath,
	}, "", "  ")
	if err != nil {
		fmt.Fprintf(app.Stderr, "encode module pin: %v\n", err)
		return ExitOperational
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "module.json"), append(pin, '\n'), 0o600); err != nil {
		fmt.Fprintf(app.Stderr, "write module pin: %v\n", err)
		return ExitOperational
	}
	if *jsonOutput {
		app.writeJSON(map[string]any{
			"kind": "Install", "path": filepath.Join(project.Directory, "modules", "@"+org, name),
			"listing": listing,
		})
		return ExitSuccess
	}
	style := newTermStyle(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Installed"), style.value("@"+org+"/"+name+"@"+listing.CapabilityVersion))
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Path"), filepath.Join(project.Directory, "modules", "@"+org, name))
	return ExitSuccess
}

func (app *App) runFork(arguments []string) int {
	flags := app.flagSet("fork")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	serverFlag := flags.String("server", "", "Doppels control-plane URL")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(app.Stderr, "fork requires @org/name")
		return ExitContract
	}
	org, name, _, err := parseHubRef(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	root, listing, code := app.fetchHubListing(org, name, *serverFlag)
	if code != ExitSuccess {
		return code
	}
	capabilityPath := filepath.Join(root, "capabilities", listing.CapabilityName+".yaml")
	recipePath := filepath.Join(root, "recipes", listing.RecipeName+".yaml")
	for _, path := range []string{capabilityPath, recipePath} {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(app.Stderr, "%s already exists; fork refuses to overwrite\n", path)
			return ExitContract
		}
	}
	if err := os.MkdirAll(filepath.Dir(capabilityPath), 0o755); err != nil {
		fmt.Fprintf(app.Stderr, "create capabilities: %v\n", err)
		return ExitOperational
	}
	if err := os.MkdirAll(filepath.Dir(recipePath), 0o755); err != nil {
		fmt.Fprintf(app.Stderr, "create recipes: %v\n", err)
		return ExitOperational
	}
	if err := os.WriteFile(capabilityPath, []byte(listing.CapabilitySource), 0o600); err != nil {
		fmt.Fprintf(app.Stderr, "write Capability: %v\n", err)
		return ExitOperational
	}
	if err := os.WriteFile(recipePath, []byte(listing.RecipeSource), 0o600); err != nil {
		fmt.Fprintf(app.Stderr, "write Recipe: %v\n", err)
		return ExitOperational
	}
	if *jsonOutput {
		app.writeJSON(map[string]any{
			"kind": "Fork", "capability": capabilityPath, "recipe": recipePath, "listing": listing,
		})
		return ExitSuccess
	}
	style := newTermStyle(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Forked"), style.value("@"+org+"/"+name))
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Capability"), capabilityPath)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Recipe"), recipePath)
	return ExitSuccess
}

func (app *App) fetchHubListing(org, name, serverFlag string) (string, *registryclient.HubListing, int) {
	workingDirectory, err := app.Getwd()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve working directory: %v\n", err)
		return "", nil, ExitOperational
	}
	root, err := project.FindRoot(workingDirectory)
	if err != nil {
		fmt.Fprintln(app.Stderr, missingLocalSpaceMessage())
		return "", nil, ExitContract
	}
	server := strings.TrimSpace(serverFlag)
	if server == "" {
		server = environmentValue(app.environment(), "DOPPELS_SERVER")
	}
	if server == "" {
		if store, storeErr := app.configStore(); storeErr == nil {
			if profile, profileErr := store.Profile(); profileErr == nil {
				server = profile.Server
			}
		}
	}
	if server == "" {
		server = "https://doppels.so"
	}
	client, err := app.registryClient(server)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return "", nil, ExitOperational
	}
	listing, err := client.GetHubListing(app.context(), org, name)
	if err != nil {
		fmt.Fprintf(app.Stderr, "hub listing: %v\n", err)
		return "", nil, ExitOperational
	}
	return root, listing, ExitSuccess
}

func (app *App) cloudOrganization() (string, configstore.Session, int) {
	session, code := app.loginSession()
	if code != ExitSuccess {
		return "", configstore.Session{}, code
	}
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return "", configstore.Session{}, ExitOperational
	}
	scope, err := store.Context()
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return "", configstore.Session{}, ExitContract
	}
	if scope.IsLocal() {
		fmt.Fprintln(app.Stderr, "publish requires a cloud Organization; run doppels org use <org>")
		return "", configstore.Session{}, ExitContract
	}
	return scope.Organization, session, ExitSuccess
}

func (app *App) writeHubResult(kind string, listing *registryclient.HubListing, jsonOutput bool) int {
	if jsonOutput {
		app.writeJSON(map[string]any{"kind": kind, "listing": listing})
		return ExitSuccess
	}
	style := newTermStyle(app.Stdout)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Status"), listing.Status)
	fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Path"), listing.PublicPath)
	return ExitSuccess
}

func parseHubRef(value string) (organization, name, version string, err error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "@") {
		return "", "", "", errors.New("hub reference must use @org/name[@version]")
	}
	rest := strings.TrimPrefix(value, "@")
	organization, nameVer, ok := strings.Cut(rest, "/")
	if !ok || organization == "" || nameVer == "" {
		return "", "", "", errors.New("hub reference must use @org/name[@version]")
	}
	name, version, _ = strings.Cut(nameVer, "@")
	if name == "" {
		return "", "", "", errors.New("hub reference must use @org/name[@version]")
	}
	return organization, name, version, nil
}
