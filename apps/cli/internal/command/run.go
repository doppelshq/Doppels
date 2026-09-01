package command

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/project"
)

type namedValues []string

func (values *namedValues) String() string { return strings.Join(*values, ",") }
func (values *namedValues) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func (app *App) runLocal(arguments []string) int {
	flags := app.flagSet("run")
	var rawInputs, rawOutputs, rawEvidence namedValues
	flags.Var(&rawInputs, "input", "Capability input as name=value (repeatable)")
	flags.Var(&rawOutputs, "output", "manual output as name=value (repeatable)")
	flags.Var(&rawEvidence, "evidence", "manual evidence as name=value (repeatable)")
	recipeName := flags.String("recipe", "", "compatible Recipe name[@version]")
	yes := flags.Bool("yes", false, "auto-approve all approval steps without prompting")
	strict := flags.Bool("strict", false, "refuse to run when a lock pin is stale")
	detach := false
	flags.BoolVar(&detach, "detach", false, "run in the background and print the Run id")
	flags.BoolVar(&detach, "d", false, "run in the background and print the Run id")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}
	if flags.NArg() > 1 {
		fmt.Fprintln(app.Stderr, "run accepts at most one capability/<name>[@version]")
		return ExitContract
	}
	worker := isDetachWorker(app.environment())
	if detach && worker {
		fmt.Fprintln(app.Stderr, "--detach cannot nest inside a detached worker")
		return ExitContract
	}

	root, catalog, code := app.localCatalog()
	if code != ExitSuccess {
		return code
	}

	interaction := newInteraction(app.Stdin, app.Stderr)
	interaction.autoYes = *yes
	capabilityRef := ""
	if flags.NArg() == 1 {
		capabilityRef = flags.Arg(0)
	} else if *jsonOutput {
		fmt.Fprintln(app.Stderr, "run requires capability/<name>[@version] when using --json")
		return ExitContract
	} else if !interaction.canPrompt() {
		fmt.Fprintln(app.Stderr, "run requires capability/<name>[@version]")
		return ExitContract
	} else {
		selected, pickErr := interaction.pickCapability(catalog, "Run a Capability")
		if pickErr != nil {
			fmt.Fprintln(app.Stderr, pickErr)
			return sharePromptExitCode(pickErr)
		}
		capabilityRef = "capability/" + selected.Value.Metadata.Name + "@" + selected.Value.Metadata.Version
	}

	capabilityDefinition, err := resolveCapabilityArgument(catalog, capabilityRef)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}

	inputs, err := parseNamedValues(rawInputs)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	if len(missingRequiredInputNames(capabilityDefinition.Value, inputs)) > 0 && !*jsonOutput && interaction.canPrompt() {
		inputs, err = interaction.askRequiredInputs(capabilityDefinition.Value, inputs)
		if err != nil {
			fmt.Fprintln(app.Stderr, err)
			return sharePromptExitCode(err)
		}
	}
	typedInputs, err := execution.ParseInputs(capabilityDefinition.Value, inputs)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	outputValues, err := parseNamedValues(rawOutputs)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	evidenceValues, err := parseNamedValues(rawEvidence)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}

	var recipeDefinition *manifest.RecipeDefinition
	selected, selectionErr := catalog.ResolveRecipe(capabilityDefinition.Value.Metadata.Name, *recipeName)
	switch {
	case selectionErr == nil:
		recipeDefinition = &selected
	case errors.Is(selectionErr, manifest.ErrRecipeNotFound) && *recipeName == "":
		// A Capability without a Recipe is fulfilled by its owner manually.
	case errors.Is(selectionErr, manifest.ErrRecipeAmbiguous) && *recipeName == "" && !*jsonOutput && interaction.canPrompt():
		picked, pickErr := interaction.pickRecipe(catalog.RecipesForCapability(capabilityDefinition.Value.Metadata.Name))
		if pickErr != nil {
			fmt.Fprintln(app.Stderr, pickErr)
			return sharePromptExitCode(pickErr)
		}
		recipeDefinition = &picked
	default:
		fmt.Fprintln(app.Stderr, selectionErr)
		if errors.Is(selectionErr, manifest.ErrRecipeAmbiguous) && *recipeName == "" {
			fmt.Fprintln(app.Stderr, "hint: pass --recipe <name>[@version], or run on a TTY without --json to choose")
		}
		return ExitContract
	}
	if recipeDefinition != nil && recipeDefinition.Value.Runtime == "shell" && (len(outputValues) > 0 || len(evidenceValues) > 0) {
		fmt.Fprintln(app.Stderr, "--output and --evidence are only valid for manual fulfillment")
		return ExitContract
	}
	if detach {
		if recipeDefinition == nil {
			fmt.Fprintln(app.Stderr, "--detach requires a shell Recipe; manual fulfillment needs a foreground terminal")
			return ExitContract
		}
		if recipeDefinition.Value.Runtime != "shell" {
			fmt.Fprintln(app.Stderr, "--detach only supports shell Recipes")
			return ExitContract
		}
	}
	issues, code := app.checkLockPin(root, capabilityDefinition, recipeDefinition, *strict)
	if code != ExitSuccess {
		return code
	}
	if detach {
		if code := app.checkRecipeDrift(recipeDefinition); code != ExitSuccess {
			return code
		}
		recipeRef := ""
		if recipeDefinition != nil {
			recipeRef = recipeDefinition.Value.Metadata.Name + "@" + recipeDefinition.Value.Metadata.Version
		}
		return app.startDetachedRun(root, buildDetachedRunArgs(capabilityRef, recipeRef, inputs, *jsonOutput), *jsonOutput)
	}
	if code := app.checkRecipeDrift(recipeDefinition); code != ExitSuccess {
		return code
	}

	identity := app.localIdentity()
	nodeID := app.localNodeID()
	invocation := execution.Invocation{
		ProjectRoot: root, Capability: capabilityDefinition.Value,
		CapabilityRef: execution.ReferenceCapability(capabilityDefinition), Inputs: typedInputs,
		RequestedBy: identity, Executor: identity, NodeID: nodeID,
	}
	if recipeDefinition != nil {
		reference := execution.ReferenceRecipe(*recipeDefinition)
		invocation.Recipe = recipeDefinition.Value
		invocation.RecipeRef = &reference
		invocation.RecipeDirectory = recipeDefinition.Source.Directory
	}
	if worker {
		if id := detachRunID(app.environment()); id != "" {
			invocation.RunID = id
		}
	}

	runtimeStdout := app.Stdout
	if *jsonOutput {
		// In JSON mode stdout is reserved for the final response document.
		runtimeStdout = app.Stderr
	} else {
		runtimeStdout = prefixLines(runtimeStdout, "    ")
	}
	// Local run is operator-initiated: invoking the command is the grant.
	// Share/Node fulfillment still prompts (or requires --yes) for Steps with approval: required.
	options := execution.Options{
		ApproveAll:  true,
		Approve:     interaction.approve,
		Manual:      interaction.manual(outputValues, evidenceValues),
		Stdout:      runtimeStdout,
		Stderr:      app.Stderr,
		Environment: app.environment(),
		Now:         app.now,
	}
	var timeline *runTimeline
	if !*jsonOutput {
		timeline = newRunTimeline(app.Stderr, invocation)
		if len(issues) > 0 {
			timeline.pinWarnings = []string{"stale"}
		}
		options.OnEvent = timeline.onEvent
	}
	result, runErr := execution.Execute(app.context(), invocation, options)

	if *jsonOutput {
		app.writeJSON(localRunView(result, runErr))
	} else {
		elapsed := time.Duration(0)
		if timeline != nil {
			elapsed = timeline.elapsed()
		}
		writeLocalRunSummary(app.Stdout, result, elapsed)
		if runErr != nil {
			fmt.Fprintf(app.Stderr, "run failed: %v\n", runErr)
		}
	}
	app.maybeFlushOutbox()
	return executionExitCode(runErr)
}

func resourceFirst(arguments []string) []string {
	if len(arguments) == 0 || strings.HasPrefix(arguments[0], "-") {
		return arguments
	}
	reordered := append([]string(nil), arguments[1:]...)
	return append(reordered, arguments[0])
}

func (app *App) localCatalog() (string, *manifest.Catalog, int) {
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
	paths, err := project.Discover(root)
	if err != nil {
		fmt.Fprintf(app.Stderr, "discover manifests: %v\n", err)
		return "", nil, ExitOperational
	}
	documents, diagnostics := load(paths)
	validation := manifest.Validate(documents, manifest.ValidationOptions{Root: root, CheckHost: false})
	diagnostics = append(diagnostics, validation.Diagnostics...)
	manifest.SortDiagnostics(diagnostics)
	if len(diagnostics) > 0 {
		for _, diagnostic := range diagnostics {
			fmt.Fprintln(app.Stderr, diagnostic.Error())
		}
		return "", nil, ExitContract
	}
	return root, validation.Catalog, ExitSuccess
}

func resolveCapabilityArgument(catalog *manifest.Catalog, resource string) (manifest.CapabilityDefinition, error) {
	kind, reference, ok := strings.Cut(resource, "/")
	if !ok || reference == "" || (kind != "capability" && kind != "capabilities") {
		return manifest.CapabilityDefinition{}, errors.New("resource must use capability/<name>[@version]")
	}
	name, version, _ := strings.Cut(reference, "@")
	return findCapability(catalog, name, version)
}

func parseNamedValues(raw []string) (map[string]string, error) {
	values := make(map[string]string, len(raw))
	for _, item := range raw {
		name, value, ok := strings.Cut(item, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("%q must use name=value", item)
		}
		if _, exists := values[name]; exists {
			return nil, fmt.Errorf("value %q was supplied more than once", name)
		}
		values[name] = value
	}
	return values, nil
}

func missingRequiredInputNames(capability *manifest.Capability, supplied map[string]string) []string {
	if capability == nil {
		return nil
	}
	names := make([]string, 0)
	for _, name := range sortedInputNames(capability.Inputs) {
		contract := capability.Inputs[name]
		if !contract.Required {
			continue
		}
		if _, exists := supplied[name]; exists {
			continue
		}
		if contract.Default != nil {
			continue
		}
		names = append(names, name)
	}
	return names
}

func sortedInputNames(values map[string]manifest.InputContract) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func buildDetachedRunArgs(capabilityRef, recipeRef string, inputs map[string]string, jsonOutput bool) []string {
	args := make([]string, 0, 4+2*len(inputs))
	if jsonOutput {
		args = append(args, "--json")
	}
	if recipeRef != "" {
		args = append(args, "--recipe", recipeRef)
	}
	for _, name := range sortedStringKeys(inputs) {
		args = append(args, "--input", name+"="+inputs[name])
	}
	args = append(args, capabilityRef)
	return args
}

func sortedStringKeys(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type interaction struct {
	reader     *bufio.Reader
	output     io.Writer
	promptable bool
	autoYes    bool
}

func newInteraction(input io.Reader, output io.Writer) *interaction {
	promptable := false
	if input == nil {
		input = strings.NewReader("")
	} else if file, ok := input.(*os.File); ok {
		fd := file.Fd()
		promptable = isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
	} else {
		// Non-file readers (tests / piped scripts) can drive numbered picks.
		promptable = true
	}
	return &interaction{reader: bufio.NewReader(input), output: output, promptable: promptable}
}

func (interaction *interaction) canPrompt() bool {
	return interaction != nil && interaction.promptable
}

func (interaction *interaction) askRequiredInputs(capability *manifest.Capability, supplied map[string]string) (map[string]string, error) {
	if capability == nil {
		return supplied, nil
	}
	result := make(map[string]string, len(supplied)+len(capability.Inputs))
	for name, value := range supplied {
		result[name] = value
	}
	style := newTermStyle(interaction.output)
	for _, name := range missingRequiredInputNames(capability, result) {
		contract := capability.Inputs[name]
		hint := contract.Type
		if len(contract.Enum) > 0 {
			parts := make([]string, 0, len(contract.Enum))
			for _, option := range contract.Enum {
				parts = append(parts, fmt.Sprint(option))
			}
			hint = strings.Join(parts, "|")
		}
		answer, err := interaction.read(fmt.Sprintf("  %s  %s (%s): ", style.field("Input"), style.cyan(name), hint))
		if err != nil {
			return nil, err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return nil, fmt.Errorf("required input %q is missing", name)
		}
		result[name] = answer
	}
	return result, nil
}

func (interaction *interaction) approve(_ context.Context, request execution.ApprovalRequest) (bool, error) {
	name := request.StepID
	if request.Name != "" {
		name = request.Name
	}
	if interaction.autoYes {
		style := newTermStyle(interaction.output)
		fmt.Fprintf(interaction.output, "  %s  %s\n", style.field("Approve"), name)
		return true, nil
	}
	style := newTermStyle(interaction.output)
	answer, err := interaction.read(fmt.Sprintf("  %s  %s? [y/N] ", style.field("Approve"), name))
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes", "s", "si", "sí":
		return true, nil
	default:
		return false, nil
	}
}

func (interaction *interaction) manual(outputs, evidence map[string]string) execution.ManualFunc {
	return func(_ context.Context, request execution.ManualRequest) (execution.ManualResult, error) {
		style := newTermStyle(interaction.output)
		if request.ProcedurePath != "" {
			fmt.Fprintf(interaction.output, "  %s  %s\n", style.field("Proc"), style.dim(request.ProcedurePath))
		} else {
			fmt.Fprintln(interaction.output, "  "+style.dim("No Recipe is available; provide the Capability outputs manually."))
		}
		returns := make(map[string]any, len(request.Capability.Outputs))
		for _, name := range sortedOutputNames(request.Capability.Outputs) {
			contract := request.Capability.Outputs[name]
			text, exists := outputs[name]
			if !exists {
				var err error
				text, err = interaction.read(fmt.Sprintf("  %s  %s (%s): ", style.field("Output"), style.cyan(name), contract.Type))
				if err != nil {
					return execution.ManualResult{}, fmt.Errorf("read output %q: %w", name, err)
				}
			}
			value, err := manualValue(text, contract.Type)
			if err != nil {
				return execution.ManualResult{}, fmt.Errorf("output %q: %w", name, err)
			}
			returns[name] = value
		}
		for name := range outputs {
			if _, exists := request.Capability.Outputs[name]; !exists {
				return execution.ManualResult{}, fmt.Errorf("undeclared output %q", name)
			}
		}

		collectedEvidence := map[string]any{}
		if request.Recipe != nil {
			for _, name := range sortedEvidenceNames(request.Recipe.Evidence) {
				contract := request.Recipe.Evidence[name]
				text, exists := evidence[name]
				if !exists {
					var err error
					text, err = interaction.read(fmt.Sprintf("  %s  %s (%s): ", style.field("Evid"), style.cyan(name), contract.Type))
					if err != nil {
						return execution.ManualResult{}, fmt.Errorf("read evidence %q: %w", name, err)
					}
				}
				value, err := manualValue(text, contract.Type)
				if err != nil {
					return execution.ManualResult{}, fmt.Errorf("evidence %q: %w", name, err)
				}
				collectedEvidence[name] = value
			}
		} else {
			for name, value := range evidence {
				if strings.HasPrefix(value, "@") {
					collectedEvidence[name] = execution.FileValue{Path: strings.TrimPrefix(value, "@")}
				} else {
					collectedEvidence[name] = value
				}
			}
		}
		return execution.ManualResult{Returns: returns, Evidence: collectedEvidence}, nil
	}
}

func (interaction *interaction) read(prompt string) (string, error) {
	fmt.Fprint(interaction.output, prompt)
	line, err := interaction.reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func manualValue(text, kind string) (any, error) {
	if kind == "artifact" {
		if text == "" {
			return nil, errors.New("file path cannot be empty")
		}
		return execution.FileValue{Path: strings.TrimPrefix(text, "@")}, nil
	}
	capability := &manifest.Capability{Inputs: map[string]manifest.InputContract{
		"value": {Type: kind, Required: true},
	}}
	parsed, err := execution.ParseInputs(capability, map[string]string{"value": text})
	if err != nil {
		return nil, err
	}
	return parsed["value"], nil
}

func sortedOutputNames(values map[string]manifest.OutputContract) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedEvidenceNames(values map[string]manifest.Evidence) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (app *App) context() context.Context {
	if app.Context != nil {
		return app.Context
	}
	return context.Background()
}

func (app *App) environment() []string {
	if app.Environment != nil {
		return app.Environment
	}
	return os.Environ()
}

func (app *App) experimentalEnabled() bool {
	for _, entry := range app.environment() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if name == "DOPPELS_EXPERIMENTAL" {
			return value == "1"
		}
	}
	// Fall back to persistent flag file written by 'doppels experimental on'.
	flagFile, err := app.experimentalFlagFile()
	if err != nil {
		return false
	}
	_, err = os.Stat(flagFile)
	return err == nil
}

func (app *App) localIdentity() execution.ActorReference {
	values := map[string]string{}
	for _, entry := range app.environment() {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	id := values["DOPPELS_IDENTITY"]
	if id == "" {
		id = values["USER"]
	}
	if id == "" {
		id = values["USERNAME"]
	}
	if id == "" {
		id = "local-user"
	}
	return execution.ActorReference{Kind: "identity", ID: id}
}

func (app *App) localNodeID() string {
	if app.Hostname != nil {
		if hostname, err := app.Hostname(); err == nil && hostname != "" {
			return hostname
		}
	}
	return "local"
}

func (app *App) now() time.Time {
	if app.Now != nil {
		return app.Now().UTC()
	}
	return time.Now().UTC()
}

func executionExitCode(err error) int {
	switch {
	case err == nil:
		return ExitSuccess
	case errors.Is(err, execution.ErrInterrupted), errors.Is(err, context.Canceled):
		return ExitInterrupted
	case errors.Is(err, execution.ErrInvalidInvocation):
		return ExitContract
	default:
		return ExitOperational
	}
}

func localRunView(result execution.Result, runErr error) map[string]any {
	view := map[string]any{
		"apiVersion": execution.APIVersion, "kind": "LocalRunResult", "status": result.Status,
		"request": result.Request, "run": result.Run, "returns": result.Returns,
		"evidence": result.Evidence, "artifacts": result.Artifacts, "stateDir": result.StateDir,
	}
	if runErr != nil {
		view["error"] = runErr.Error()
	}
	return view
}
