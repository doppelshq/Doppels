package command

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/localregistry"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
	"doppels.so/cli/internal/projectlock"
	"doppels.so/cli/internal/registryclient"
	"gopkg.in/yaml.v3"
)

type reconcileInput struct {
	root    string
	session configstore.Session
	scope   configstore.Context
	body    registryclient.ReconcileRequest
	lock    projectlock.File
	local   bool
}

// recipeRegistryDescriptor is deliberately not the executable Recipe. Cloud
// can discover and govern the implementation revision without receiving
// scripts, environment mappings, procedures or return wiring.
type recipeRegistryDescriptor struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Metadata   manifest.Metadata      `json:"metadata"`
	Provides   []string               `json:"provides"`
	Runtime    string                 `json:"runtime"`
	Requires   *manifest.Requirements `json:"requires,omitempty"`
}

const maxManifestSourceBytes = 1_000_000

func (app *App) runPreview(arguments []string) int {
	flags := app.flagSet("preview")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	var spaceFile string
	flags.StringVar(&spaceFile, "f", "", "Space manifest (defaults to doppels.<space>.yaml when present)")
	flags.StringVar(&spaceFile, "file", "", "Space manifest (defaults to doppels.<space>.yaml when present)")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "preview accepts a Space manifest through -f; definitions are discovered from the local Space")
		return ExitContract
	}
	input, code := app.reconcileInput(spaceFile)
	if code != ExitSuccess {
		return code
	}
	if input.local {
		return app.previewLocal(input, *jsonOutput)
	}
	client, err := app.registryClient(input.session.Profile.Server)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitOperational
	}
	response, err := client.Preview(app.context(), input.session.Token, input.scope.Organization, input.scope.Space, input.body)
	if err != nil {
		fmt.Fprintf(app.Stderr, "preview: %v\n", err)
		return reconcileErrorCode(err)
	}
	server := input.session.Profile.Server
	if *jsonOutput {
		app.writeJSON(map[string]any{
			"kind": "Preview", "server": server, "context": input.scope, "changes": response.Changes,
			"applicable": response.Applicable,
		})
	} else {
		writeChanges(app.Stdout, server, input.scope, response.Changes, input.body.Resources)
		if !response.Applicable {
			fmt.Fprintln(app.Stderr, "Preview contains conflicts and cannot be applied")
			writeApplyConflictHints(app.Stderr, input.scope, response.Changes)
		}
	}
	if !response.Applicable {
		return ExitContract
	}
	return ExitSuccess
}

func (app *App) previewLocal(input reconcileInput, jsonOutput bool) int {
	previous, err := localregistry.Load(input.root)
	if err != nil {
		fmt.Fprintf(app.Stderr, "load local registry: %v\n", err)
		return ExitOperational
	}
	changes := localregistry.DiffChanges(previous.Resources, input.body.Resources)
	if jsonOutput {
		app.writeJSON(map[string]any{
			"kind": "Preview", "server": "local", "context": input.scope, "changes": changes, "applicable": true,
		})
	} else {
		writeChanges(app.Stdout, "local", input.scope, changes, input.body.Resources)
	}
	return ExitSuccess
}

func (app *App) runApply(arguments []string) int {
	flags := app.flagSet("apply")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	var spaceFile string
	flags.StringVar(&spaceFile, "f", "", "Space manifest (defaults to doppels.<space>.yaml when present)")
	flags.StringVar(&spaceFile, "file", "", "Space manifest (defaults to doppels.<space>.yaml when present)")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "apply accepts a Space manifest through -f; definitions are discovered from the local Space")
		return ExitContract
	}
	input, code := app.reconcileInput(spaceFile)
	if code != ExitSuccess {
		return code
	}
	pendingLock, err := projectlock.Prepare(input.root, input.lock)
	if err != nil {
		fmt.Fprintf(app.Stderr, "prepare %s before apply: %v\n", projectlock.Filename, err)
		return ExitOperational
	}
	defer pendingLock.Abort()
	if input.local {
		return app.applyLocal(input, pendingLock, *jsonOutput)
	}
	client, err := app.registryClient(input.session.Profile.Server)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitOperational
	}
	response, err := client.Apply(app.context(), input.session.Token, input.scope.Organization, input.scope.Space, input.body)
	if err != nil {
		var conflict registryclient.RegistryConflictError
		if errors.As(err, &conflict) {
			if *jsonOutput {
				app.writeJSON(map[string]any{
					"kind": "Apply", "server": input.session.Profile.Server, "context": input.scope,
					"changes": conflict.Changes, "applied": false,
				})
			} else {
				writeChanges(app.Stdout, input.session.Profile.Server, input.scope, conflict.Changes, input.body.Resources)
				fmt.Fprintln(app.Stderr, "Apply contains conflicts")
				writeApplyConflictHints(app.Stderr, input.scope, conflict.Changes)
			}
			return ExitContract
		}
		fmt.Fprintf(app.Stderr, "apply: %v\n", err)
		return reconcileErrorCode(err)
	}
	if !response.Applied {
		fmt.Fprintln(app.Stderr, "Cloud did not confirm the atomic apply")
		return ExitOperational
	}
	server := input.session.Profile.Server
	if err := pendingLock.Commit(); err != nil {
		if *jsonOutput {
			app.writeJSON(map[string]any{
				"kind": "Apply", "server": server, "context": input.scope, "changes": response.Changes,
				"applied": true, "lockWritten": false, "error": err.Error(),
			})
		} else {
			fmt.Fprintf(app.Stderr, "Cloud apply succeeded, but writing %s failed: %v; fix local filesystem access and run doppels apply again\n", projectlock.Filename, err)
		}
		return ExitOperational
	}
	_ = app.recordSpaceBinding(input.scope, input.root)
	if *jsonOutput {
		app.writeJSON(map[string]any{
			"kind": "Apply", "server": server, "context": input.scope, "changes": response.Changes,
			"applied": true, "lockFile": projectlock.Filename,
		})
	} else {
		writeChanges(app.Stdout, server, input.scope, response.Changes, input.body.Resources)
		fmt.Fprintf(app.Stdout, "  %s  %s\n", newTermStyle(app.Stdout).field("Lock"), projectlock.Filename)
	}
	return ExitSuccess
}

func (app *App) applyLocal(input reconcileInput, pendingLock *projectlock.Pending, jsonOutput bool) int {
	previous, err := localregistry.Load(input.root)
	if err != nil {
		fmt.Fprintf(app.Stderr, "load local registry: %v\n", err)
		return ExitOperational
	}
	changes := localregistry.DiffChanges(previous.Resources, input.body.Resources)
	if err := localregistry.Write(input.root, input.scope.Space, input.body.Resources); err != nil {
		fmt.Fprintf(app.Stderr, "write local registry: %v\n", err)
		return ExitOperational
	}
	if err := pendingLock.Commit(); err != nil {
		fmt.Fprintf(app.Stderr, "write %s: %v\n", projectlock.Filename, err)
		return ExitOperational
	}
	_ = app.recordSpaceBinding(input.scope, input.root)
	if jsonOutput {
		app.writeJSON(map[string]any{
			"kind": "Apply", "server": "local", "context": input.scope, "changes": changes,
			"applied": true, "lockFile": projectlock.Filename,
		})
	} else {
		writeChanges(app.Stdout, "local", input.scope, changes, input.body.Resources)
		fmt.Fprintf(app.Stdout, "  %s  %s\n", newTermStyle(app.Stdout).field("Lock"), projectlock.Filename)
	}
	return ExitSuccess
}

func (app *App) recordSpaceBinding(scope configstore.Context, root string) error {
	store, err := app.configStore()
	if err != nil {
		return err
	}
	return store.SetBinding(scope.Organization, scope.Space, root)
}

func (app *App) reconcileInput(explicitSpaceFile string) (reconcileInput, int) {
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return reconcileInput{}, ExitOperational
	}
	scope, err := store.Context()
	if err != nil || !scope.Valid() || scope.Space == "" {
		fmt.Fprintln(app.Stderr, "preview/apply require a Space; run doppels org use <organization> and doppels space use <space>")
		return reconcileInput{}, ExitContract
	}
	local := scope.IsLocal()
	var session configstore.Session
	if !local {
		sess, code := app.loginSession()
		if code != ExitSuccess {
			return reconcileInput{}, code
		}
		session = sess
	}
	root, catalog, code := app.localCatalog()
	if code != ExitSuccess {
		return reconcileInput{}, code
	}
	if err := ensureSingleRegistryRevision(catalog); err != nil {
		fmt.Fprintln(app.Stderr, err)
		return reconcileInput{}, ExitContract
	}
	body := registryclient.ReconcileRequest{Resources: []registryclient.Resource{}}
	spacePath, found := explicitSpaceFile, explicitSpaceFile != ""
	if found {
		spacePath, err = filepath.Abs(spacePath)
		if err != nil {
			fmt.Fprintf(app.Stderr, "resolve Space manifest: %v\n", err)
			return reconcileInput{}, ExitOperational
		}
	} else {
		spacePath, found, err = project.FindSpaceManifest(root, scope.Space)
		if err != nil {
			fmt.Fprintf(app.Stderr, "resolve Space manifest: %v\n", err)
			return reconcileInput{}, ExitOperational
		}
	}
	if found {
		loaded, err := manifest.Load(spacePath)
		if err != nil {
			fmt.Fprintf(app.Stderr, "load Space manifest: %v\n", err)
			return reconcileInput{}, ExitContract
		}
		space, ok := loaded.Document.(*manifest.Space)
		if !ok {
			fmt.Fprintf(app.Stderr, "%s must contain kind Space\n", spacePath)
			return reconcileInput{}, ExitContract
		}
		validation := manifest.Validate([]manifest.Loaded{loaded}, manifest.ValidationOptions{Root: root})
		if len(validation.Diagnostics) > 0 {
			for _, diagnostic := range validation.Diagnostics {
				fmt.Fprintln(app.Stderr, diagnostic.Error())
			}
			return reconcileInput{}, ExitContract
		}
		if space.Metadata.Name != scope.Space {
			fmt.Fprintf(app.Stderr, "Space manifest metadata.name %q does not match Context Space %q\n", space.Metadata.Name, scope.Space)
			return reconcileInput{}, ExitContract
		}
		body.Space = &registryclient.SpaceConfiguration{
			SourceAuthority: "manifest",
			Metadata: registryclient.ScopeMetadata{
				Name: space.Metadata.Name, DisplayName: space.Metadata.DisplayName, Summary: space.Metadata.Summary,
				Description: space.Metadata.Description, Labels: space.Metadata.Labels,
				Annotations: space.Metadata.Annotations,
			},
		}
	}

	lockEntries := make([]projectlock.Entry, 0)
	capabilityNames := sortedKeys(catalog.Capabilities)
	for _, name := range capabilityNames {
		for _, definition := range catalog.Capabilities[name] {
			reference := execution.ReferenceCapability(definition)
			source, err := os.ReadFile(definition.Source.Path)
			if err != nil {
				fmt.Fprintf(app.Stderr, "read Capability source: %v\n", err)
				return reconcileInput{}, ExitOperational
			}
			if len(source) == 0 || len(source) > maxManifestSourceBytes {
				fmt.Fprintf(app.Stderr, "Capability source %s must contain between 1 and %d bytes\n", definition.Source.Path, maxManifestSourceBytes)
				return reconcileInput{}, ExitContract
			}
			var projection map[string]any
			if err := yaml.Unmarshal(source, &projection); err != nil {
				fmt.Fprintf(app.Stderr, "parse Capability source %s: %v\n", definition.Source.Path, err)
				return reconcileInput{}, ExitContract
			}
			body.Resources = append(body.Resources, registryclient.Resource{
				Kind: "Capability", SourceAuthority: "manifest", Revision: reference,
				Manifest: projection, ManifestSource: string(source),
			})
			lockEntries = append(lockEntries, projectlock.Entry{Kind: "Capability", SourceAuthority: "manifest", Revision: reference})
		}
	}
	recipeNames := sortedKeys(catalog.Recipes)
	for _, name := range recipeNames {
		for _, definition := range catalog.Recipes[name] {
			reference := execution.ReferenceRecipe(definition)
			body.Resources = append(body.Resources, registryclient.Resource{
				Kind: "Recipe", SourceAuthority: "manifest", Revision: reference,
				Manifest: recipeRegistryDescriptor{
					APIVersion: definition.Value.APIVersion, Kind: definition.Value.Kind,
					Metadata: definition.Value.Metadata, Provides: definition.Value.Provides,
					Runtime: definition.Value.Runtime, Requires: definition.Value.Requires,
				},
			})
			lockEntries = append(lockEntries, projectlock.Entry{Kind: "Recipe", SourceAuthority: "manifest", Revision: reference})
		}
	}
	desiredLock := projectlock.New(lockEntries)
	existingLock, err := projectlock.Load(root)
	if err != nil {
		fmt.Fprintf(app.Stderr, "load %s: %v\n", projectlock.Filename, err)
		return reconcileInput{}, ExitOperational
	}
	if err := projectlock.Verify(existingLock, desiredLock); err != nil {
		fmt.Fprintf(app.Stderr, "%s conflict: %v\n", projectlock.Filename, err)
		return reconcileInput{}, ExitContract
	}
	return reconcileInput{
		root: root, session: session, scope: scope, body: body, lock: desiredLock, local: local,
	}, ExitSuccess
}

func ensureSingleRegistryRevision(catalog *manifest.Catalog) error {
	for _, kind := range []struct {
		name      string
		revisions map[string]int
	}{
		{name: "Capability", revisions: revisionCounts(catalog.Capabilities)},
		{name: "Recipe", revisions: revisionCounts(catalog.Recipes)},
	} {
		names := make([]string, 0, len(kind.revisions))
		for name, count := range kind.revisions {
			if count > 1 {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			return fmt.Errorf("preview/apply found multiple local revisions of %s %q; select exactly one revision in the local Space", kind.name, names[0])
		}
	}
	return nil
}

func revisionCounts[T any](values map[string][]T) map[string]int {
	counts := make(map[string]int, len(values))
	for name, revisions := range values {
		counts[name] = len(revisions)
	}
	return counts
}

func sortedKeys[T any](values map[string][]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func writeChanges(writer io.Writer, server string, scope configstore.Context, changes []registryclient.Change, resources []registryclient.Resource) {
	style := newTermStyle(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Cloud"), style.value(server))
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Scope"), style.value(scope.String()))
	if len(changes) == 0 || allChangesUnchanged(changes) {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Preview"), style.dim("Already in sync"))
		return
	}
	fmt.Fprintln(writer)
	rows := [][]string{{
		style.dim("ACTION"), style.dim("RESOURCE"), style.dim("REASON"),
	}}
	for _, row := range hierarchicalChangeRows(style, changes, resources) {
		rows = append(rows, row)
	}
	writeAlignedColumns(writer, rows)
}

func allChangesUnchanged(changes []registryclient.Change) bool {
	for _, change := range changes {
		switch normalizeChangeAction(change.Action) {
		case "none", "noop":
			continue
		default:
			return false
		}
	}
	return len(changes) > 0
}

func hierarchicalChangeRows(style termStyle, changes []registryclient.Change, resources []registryclient.Resource) [][]string {
	provides := recipeProvidesFromResources(resources)
	var organizations, spaces, capabilities, recipes, other []registryclient.Change
	for _, change := range changes {
		switch strings.ToLower(change.Kind) {
		case "organization":
			organizations = append(organizations, change)
		case "space":
			spaces = append(spaces, change)
		case "capability":
			capabilities = append(capabilities, change)
		case "recipe":
			recipes = append(recipes, change)
		default:
			other = append(other, change)
		}
	}
	sortChanges(organizations)
	sortChanges(spaces)
	sortChanges(capabilities)
	sortChanges(recipes)
	sortChanges(other)

	linked := map[string]struct{}{}
	rows := make([][]string, 0, len(changes))
	appendChange := func(change registryclient.Change, prefix string) {
		action := displayChangeAction(change.Action)
		rows = append(rows, []string{
			colorChangeAction(style, action),
			prefix + changeResourceRef(change),
			style.dim(humanChangeReason(change.Action, change.Reason)),
		})
	}
	for _, change := range organizations {
		appendChange(change, "")
	}
	for _, change := range spaces {
		appendChange(change, "")
	}
	for _, capability := range capabilities {
		appendChange(capability, "")
		children := make([]registryclient.Change, 0)
		for _, recipe := range recipes {
			for _, provided := range provides[recipe.Name] {
				if provided == capability.Name {
					children = append(children, recipe)
					linked[changeKey(recipe)] = struct{}{}
					break
				}
			}
		}
		sortChanges(children)
		for index, recipe := range children {
			prefix := "├─ "
			if index == len(children)-1 {
				prefix = "└─ "
			}
			appendChange(recipe, style.dim(prefix))
		}
	}
	for _, recipe := range recipes {
		if _, ok := linked[changeKey(recipe)]; ok {
			continue
		}
		appendChange(recipe, "")
	}
	for _, change := range other {
		appendChange(change, "")
	}
	return rows
}

func recipeProvidesFromResources(resources []registryclient.Resource) map[string][]string {
	out := map[string][]string{}
	for _, resource := range resources {
		if !strings.EqualFold(resource.Kind, "Recipe") {
			continue
		}
		provides := extractProvides(resource.Manifest)
		if len(provides) == 0 {
			continue
		}
		out[resource.Revision.Name] = provides
	}
	return out
}

func extractProvides(manifest any) []string {
	switch value := manifest.(type) {
	case recipeRegistryDescriptor:
		return append([]string(nil), value.Provides...)
	case map[string]any:
		raw, _ := value["provides"].([]any)
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func sortChanges(changes []registryclient.Change) {
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Name != changes[j].Name {
			return changes[i].Name < changes[j].Name
		}
		return changes[i].Version < changes[j].Version
	})
}

func changeKey(change registryclient.Change) string {
	return strings.ToLower(change.Kind) + "/" + change.Name + "@" + change.Version
}

func changeResourceRef(change registryclient.Change) string {
	ref := strings.ToLower(change.Kind) + "/" + change.Name
	if change.Version != "" {
		ref += "@" + change.Version
	}
	return ref
}

func normalizeChangeAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "noop", "none":
		return "none"
	default:
		return action
	}
}

func displayChangeAction(action string) string {
	switch normalizeChangeAction(action) {
	case "none", "noop":
		return "unchanged"
	default:
		return action
	}
}

func humanChangeReason(action, reason string) string {
	switch reason {
	case "missing":
		return "new"
	case "unchanged":
		return "—"
	case "revision_missing":
		return "new revision"
	case "space_registration_missing":
		return "register"
	case "active_revision_changed":
		return "revision"
	case "configuration_changed":
		return "updated"
	case "":
		switch normalizeChangeAction(action) {
		case "create":
			return "new"
		case "none", "noop":
			return "—"
		case "update":
			return "updated"
		default:
			return "—"
		}
	default:
		return reason
	}
}

func writeApplyConflictHints(writer io.Writer, scope configstore.Context, changes []registryclient.Change) {
	style := newTermStyle(writer)
	seen := map[string]struct{}{}
	for _, change := range changes {
		if !strings.EqualFold(change.Action, "conflict") {
			continue
		}
		key := change.Kind + ":" + change.Reason
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		switch change.Reason {
		case "space_manifest_required":
			fmt.Fprintf(writer, "  %s  %s\n", style.field("Next"), style.dim(fmt.Sprintf(
				"create doppels.%s.yaml (`doppels init %s`), then apply again",
				scope.Space, scope.Space,
			)))
		case "source_authority_mismatch":
			if strings.EqualFold(change.Kind, "Space") {
				fmt.Fprintf(writer, "  %s  %s\n", style.field("Next"), style.dim(fmt.Sprintf(
					"Space %q is cloud-managed; pick another Space name with a local doppels.<space>.yaml",
					change.Name,
				)))
			} else {
				fmt.Fprintf(writer, "  %s  %s\n", style.field("Next"), style.dim(fmt.Sprintf(
					"%s %q is managed by another authority; rename locally or leave Cloud as source of truth",
					change.Kind, change.Name,
				)))
			}
		}
	}
}

func colorChangeAction(style termStyle, action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "create":
		return style.green(action)
	case "update":
		return style.cyan(action)
	case "delete", "unregister", "prune":
		return style.red(action)
	case "conflict":
		return style.yellow(action)
	case "unchanged", "none", "noop":
		return style.dim(action)
	default:
		return action
	}
}

func reconcileErrorCode(err error) int {
	var problem registryclient.HTTPError
	if errors.As(err, &problem) && (problem.StatusCode == http.StatusBadRequest || problem.StatusCode == http.StatusNotFound || problem.StatusCode == http.StatusConflict || problem.StatusCode == http.StatusRequestEntityTooLarge || problem.StatusCode == http.StatusUnprocessableEntity) {
		return ExitContract
	}
	return ExitOperational
}
