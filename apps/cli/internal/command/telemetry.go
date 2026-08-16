package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/telemetry"
)

func (app *App) runTelemetry(arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(app.Stderr, "usage: doppels telemetry <accept|reject|status> [--json]")
		return ExitContract
	}
	switch arguments[0] {
	case "accept", "enable":
		return app.runTelemetryAccept(arguments[1:])
	case "reject", "disable":
		return app.runTelemetryReject(arguments[1:])
	case "status":
		return app.runTelemetryStatus(arguments[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(app.Stdout, "usage: doppels telemetry <accept|reject|status> [--json]")
		fmt.Fprintln(app.Stdout, "  accept   opt in to anonymous product telemetry (future PostHog)")
		fmt.Fprintln(app.Stdout, "  reject   opt out / disable persisted preference")
		fmt.Fprintln(app.Stdout, "  status   show effective preference (env overrides profile)")
		fmt.Fprintln(app.Stdout, "DOPPELS_TELEMETRY=0 disables shipping even after accept.")
		return ExitSuccess
	default:
		fmt.Fprintf(app.Stderr, "unknown telemetry subcommand %q\n", arguments[0])
		fmt.Fprintln(app.Stderr, "usage: doppels telemetry <accept|reject|status> [--json]")
		return ExitContract
	}
}

func (app *App) telemetryNow() time.Time {
	if app.Now != nil {
		return app.Now()
	}
	return time.Now()
}

func (app *App) runTelemetryAccept(arguments []string) int {
	flags := app.flagSet("telemetry accept")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return ExitOperational
	}
	if err := store.SetTelemetry(true, app.telemetryNow()); err != nil {
		fmt.Fprintf(app.Stderr, "save telemetry preference: %v\n", err)
		return ExitOperational
	}
	fmt.Fprintln(app.Stdout, "Telemetry enabled. Anonymous product events may be sent later; DOPPELS_TELEMETRY=0 disables.")
	return ExitSuccess
}

func (app *App) runTelemetryReject(arguments []string) int {
	flags := app.flagSet("telemetry reject")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return ExitOperational
	}
	if err := store.SetTelemetry(false, app.telemetryNow()); err != nil {
		fmt.Fprintf(app.Stderr, "save telemetry preference: %v\n", err)
		return ExitOperational
	}
	fmt.Fprintln(app.Stdout, "Telemetry disabled.")
	return ExitSuccess
}

func (app *App) runTelemetryStatus(arguments []string) int {
	flags := app.flagSet("telemetry status")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return ExitOperational
	}
	prefs := configstore.Telemetry{}
	if profile, profileErr := store.Profile(); profileErr == nil {
		prefs = profile.Telemetry
	} else if !errors.Is(profileErr, configstore.ErrNotConfigured) {
		fmt.Fprintf(app.Stderr, "read CLI profile: %v\n", profileErr)
		return ExitOperational
	}
	decision := telemetry.Decide(app.environment(), prefs)
	if *asJSON {
		payload := map[string]any{
			"kind":    "TelemetryStatus",
			"enabled": decision.Enabled,
			"source":  decision.Source,
		}
		if !prefs.AcceptedAt.IsZero() {
			payload["acceptedAt"] = prefs.AcceptedAt.UTC().Format(time.RFC3339Nano)
		}
		if !prefs.RejectedAt.IsZero() {
			payload["rejectedAt"] = prefs.RejectedAt.UTC().Format(time.RFC3339Nano)
		}
		encoded, encErr := json.MarshalIndent(payload, "", "  ")
		if encErr != nil {
			fmt.Fprintf(app.Stderr, "encode status: %v\n", encErr)
			return ExitOperational
		}
		fmt.Fprintln(app.Stdout, string(encoded))
		return ExitSuccess
	}
	state := "disabled"
	if decision.Enabled {
		state = "enabled"
	}
	fmt.Fprintf(app.Stdout, "Telemetry %s (source: %s).\n", state, decision.Source)
	return ExitSuccess
}
