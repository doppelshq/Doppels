package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (app *App) runExperimental(arguments []string) int {
	if isHelp(arguments) {
		fmt.Fprintln(app.Stdout, "Usage: doppels experimental [on|off|status] [--json]")
		return ExitSuccess
	}
	flags := app.flagSet("experimental")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(resourceFirst(arguments)); err != nil {
		return ExitContract
	}

	sub := ""
	if flags.NArg() == 1 {
		sub = strings.ToLower(flags.Arg(0))
	} else if flags.NArg() > 1 {
		fmt.Fprintln(app.Stderr, "experimental accepts: on, off, or status")
		return ExitContract
	}

	flagFile, err := app.experimentalFlagFile()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve config dir: %v\n", err)
		return ExitOperational
	}

	switch sub {
	case "on":
		if err := os.MkdirAll(filepath.Dir(flagFile), 0o700); err != nil {
			fmt.Fprintf(app.Stderr, "create config dir: %v\n", err)
			return ExitOperational
		}
		if err := os.WriteFile(flagFile, []byte("1\n"), 0o600); err != nil {
			fmt.Fprintf(app.Stderr, "write experimental flag: %v\n", err)
			return ExitOperational
		}
		return app.writeExperimentalStatus(true, *jsonOutput)

	case "off":
		_ = os.Remove(flagFile)
		return app.writeExperimentalStatus(false, *jsonOutput)

	case "", "status":
		active := app.experimentalEnabled()
		return app.writeExperimentalStatus(active, *jsonOutput)

	default:
		fmt.Fprintf(app.Stderr, "unknown subcommand %q; use on, off, or status\n", sub)
		return ExitContract
	}
}

func (app *App) writeExperimentalStatus(active bool, jsonOutput bool) int {
	if jsonOutput {
		app.writeJSON(map[string]any{"experimental": active})
		return ExitSuccess
	}
	style := newTermStyle(app.Stdout)
	fmt.Fprintln(app.Stdout)
	if active {
		fmt.Fprintf(app.Stdout, "  %s  on\n", style.field("Experimental"))
		fmt.Fprintf(app.Stdout, "  %s\n", style.dim("Preview features are enabled. Set DOPPELS_EXPERIMENTAL=0 to override per-session."))
	} else {
		fmt.Fprintf(app.Stdout, "  %s  off\n", style.field("Experimental"))
		fmt.Fprintf(app.Stdout, "  %s\n", style.dim("Run 'doppels experimental on' to enable preview features."))
	}
	fmt.Fprintln(app.Stdout)
	return ExitSuccess
}

func (app *App) experimentalFlagFile() (string, error) {
	store, err := app.configStore()
	if err != nil {
		return "", err
	}
	return filepath.Join(store.Dir(), "experimental"), nil
}
