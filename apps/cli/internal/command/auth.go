package command

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/registryclient"
)

const defaultCloudServer = "https://doppels.so"

func nonDefaultCloud(server string) bool {
	left := strings.TrimRight(strings.TrimSpace(server), "/")
	right := strings.TrimRight(defaultCloudServer, "/")
	return left != "" && left != right
}

func (app *App) configStore() (*configstore.Store, error) {
	if configured := environmentValue(app.environment(), "DOPPELS_CONFIG_HOME"); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return nil, err
		}
		return configstore.New(absolute), nil
	}
	if app.ConfigDir != nil {
		directory, err := app.ConfigDir()
		if err != nil {
			return nil, err
		}
		return configstore.New(directory), nil
	}
	directory, err := configstore.DefaultDir()
	if err != nil {
		return nil, err
	}
	return configstore.New(directory), nil
}

func (app *App) registryClient(server string) (*registryclient.Client, error) {
	return registryclient.New(server, app.HTTPClient)
}

func (app *App) runLogin(arguments []string) int {
	flags := app.flagSet("login")
	defaultServer := environmentValue(app.environment(), "DOPPELS_SERVER")
	if defaultServer == "" {
		// Logout deliberately preserves the non-secret profile. Reuse its
		// endpoint on the next login so a custom control plane does not
		// silently fall back to doppels.so.
		if store, err := app.configStore(); err == nil {
			if profile, err := store.Profile(); err == nil {
				defaultServer = profile.Server
			}
		}
		if defaultServer == "" {
			defaultServer = defaultCloudServer
		}
	}
	server := flags.String("server", defaultServer, "Doppels control-plane URL")
	token := flags.String("token", environmentValue(app.environment(), "DOPPELS_API_TOKEN"), "API token (optional; omit to open browser login)")
	tokenStdin := flags.Bool("token-stdin", false, "read the API token from stdin without an interactive echoing prompt")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "login accepts no positional arguments")
		return ExitContract
	}
	if *tokenStdin && *token != "" {
		fmt.Fprintln(app.Stderr, "--token-stdin cannot be combined with --token or DOPPELS_API_TOKEN")
		return ExitContract
	}
	if *tokenStdin {
		if app.Stdin == nil {
			fmt.Fprintln(app.Stderr, "--token-stdin requires a token on stdin")
			return ExitContract
		}
		reader := bufio.NewReader(app.Stdin)
		value, err := reader.ReadString('\n')
		if err != nil && value == "" {
			fmt.Fprintln(app.Stderr, "--token-stdin requires a token on stdin")
			return ExitContract
		}
		*token = strings.TrimSpace(value)
	}
	parsedServer, err := registryclient.ParseServer(*server)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	canonicalServer := parsedServer.String()
	client, err := app.registryClient(canonicalServer)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	if strings.TrimSpace(*token) == "" {
		return app.runBrowserLogin(client, canonicalServer, *jsonOutput)
	}
	remote, err := client.Session(app.context(), *token)
	if err != nil {
		fmt.Fprintf(app.Stderr, "login failed: %v\n", err)
		return ExitOperational
	}
	return app.persistLogin(canonicalServer, *token, remote, *jsonOutput)
}

func (app *App) runBrowserLogin(client *registryclient.Client, canonicalServer string, jsonOutput bool) int {
	challenge, err := client.StartDeviceLogin(app.context())
	if err != nil {
		fmt.Fprintf(app.Stderr, "start browser login: %v\n", err)
		return ExitOperational
	}
	verification := challenge.VerificationURIComplete
	if verification == "" {
		verification = challenge.VerificationURI
	}
	if !jsonOutput {
		style := newTermStyle(app.Stdout)
		fmt.Fprintln(app.Stdout)
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Login"), style.boldCyan(verification))
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Code"), style.value(challenge.UserCode))
		fmt.Fprintln(app.Stdout)
	} else {
		fmt.Fprintf(app.Stderr, "Open %s (user code %s) and approve this CLI\n", verification, challenge.UserCode)
	}

	deadline := app.now().Add(time.Duration(challenge.ExpiresIn) * time.Second)
	interval := time.Duration(challenge.Interval) * time.Second
	if interval < time.Second {
		interval = 2 * time.Second
	}

	var waitSpin *waitSpinner
	if !jsonOutput {
		waitSpin = startWaitSpinner(app.Stderr, "waiting for Cloud approval")
	}
	defer waitSpin.Stop()

	for {
		if app.now().After(deadline) {
			waitSpin.Stop()
			fmt.Fprintln(app.Stderr, "browser login expired; run doppels login again")
			return ExitOperational
		}
		poll, err := client.PollDeviceLogin(app.context(), challenge.DeviceCode)
		if err != nil {
			var httpErr registryclient.HTTPError
			if errors.As(err, &httpErr) {
				switch httpErr.Code {
				case "authorization_pending":
					// keep waiting
				case "expired", "already_consumed":
					waitSpin.Stop()
					fmt.Fprintf(app.Stderr, "browser login %s; run doppels login again\n", httpErr.Code)
					return ExitOperational
				case "denied":
					waitSpin.Stop()
					fmt.Fprintln(app.Stderr, "browser login denied")
					return ExitOperational
				default:
					waitSpin.Stop()
					fmt.Fprintf(app.Stderr, "browser login poll: %v\n", err)
					return ExitOperational
				}
			} else {
				waitSpin.Stop()
				fmt.Fprintf(app.Stderr, "browser login poll: %v\n", err)
				return ExitOperational
			}
		} else if poll != nil && poll.Status == "approved" {
			waitSpin.Stop()
			remote := &registryclient.SessionResponse{
				APIVersion: "doppels.so/v1alpha1",
				Identity:   poll.Identity,
				Personal:   poll.Personal,
			}
			return app.persistLogin(canonicalServer, poll.Token, remote, jsonOutput)
		} else if poll != nil && poll.Status == "authorization_pending" {
			// keep waiting
		} else {
			waitSpin.Stop()
			fmt.Fprintln(app.Stderr, "browser login returned an unexpected poll response")
			return ExitOperational
		}

		timer := time.NewTimer(interval)
		select {
		case <-app.context().Done():
			timer.Stop()
			waitSpin.Stop()
			fmt.Fprintln(app.Stderr, "browser login interrupted")
			return ExitInterrupted
		case <-timer.C:
		}
	}
}

func (app *App) persistLogin(canonicalServer, token string, remote *registryclient.SessionResponse, jsonOutput bool) int {
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return ExitOperational
	}
	if err := store.Login(canonicalServer, token, app.now()); err != nil {
		fmt.Fprintf(app.Stderr, "save login: %v\n", err)
		return ExitOperational
	}
	selectedContext, err := app.applyPersonalContext(store, remote)
	if err != nil {
		fmt.Fprintf(app.Stderr, "set personal Context: %v\n", err)
		return ExitOperational
	}
	view := map[string]any{"kind": "Login", "server": canonicalServer, "identity": remote.Identity}
	if selectedContext != nil {
		view["context"] = *selectedContext
	}
	if remote.Personal != nil {
		view["personal"] = remote.Personal
	}
	if jsonOutput {
		app.writeJSON(view)
	} else {
		ctx := configstore.Context{}
		if selectedContext != nil {
			ctx = *selectedContext
		}
		writeSessionSummary(app.Stdout, remote.Identity, canonicalServer, ctx)
		if selectedContext != nil {
			style := newTermStyle(app.Stdout)
			fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Next"), style.dim("doppels organizations · doppels org use <organization>"))
		}
	}
	app.maybeFlushOutbox()
	return ExitSuccess
}

// applyPersonalContext selects the Cloud personal Org/Space after login.
// Always resets Context so a previous Org (e.g. local/platform) cannot linger
// when the new Identity has no membership there.
func (app *App) applyPersonalContext(store *configstore.Store, remote *registryclient.SessionResponse) (*configstore.Context, error) {
	if remote == nil || remote.Personal == nil {
		return nil, nil
	}
	selected := configstore.Context{
		Organization: remote.Personal.Organization,
		Space:        remote.Personal.Space,
	}
	if !selected.Valid() {
		return nil, errors.New("Cloud personal scope is not a valid Context")
	}
	if err := store.SetContext(selected); err != nil {
		return nil, err
	}
	return &selected, nil
}

func (app *App) runLogout(arguments []string) int {
	flags := app.flagSet("logout")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "logout accepts no arguments")
		return ExitContract
	}
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return ExitOperational
	}
	if err := store.Logout(); err != nil {
		fmt.Fprintf(app.Stderr, "logout: %v\n", err)
		return ExitOperational
	}
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "Logout", "status": "logged_out"})
	} else {
		style := newTermStyle(app.Stdout)
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Status"), style.bold("Logged out"))
	}
	return ExitSuccess
}

func (app *App) runWhoAmI(arguments []string) int {
	flags := app.flagSet("whoami")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "whoami accepts no arguments")
		return ExitContract
	}
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return ExitOperational
	}
	session, err := store.Session()
	if err != nil {
		if errors.Is(err, configstore.ErrNotLoggedIn) {
			writeNotLoggedIn(app.Stderr)
			return ExitContract
		}
		fmt.Fprintf(app.Stderr, "load login: %v\n", err)
		return ExitOperational
	}
	client, err := app.registryClient(session.Profile.Server)
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitOperational
	}
	remote, err := client.Session(app.context(), session.Token)
	if err != nil {
		fmt.Fprintf(app.Stderr, "inspect login: %v\n", err)
		return ExitOperational
	}
	view := map[string]any{"kind": "CurrentIdentity", "server": session.Profile.Server, "identity": remote.Identity, "context": session.Profile.Context}
	if *jsonOutput {
		app.writeJSON(view)
	} else {
		writeSessionSummary(app.Stdout, remote.Identity, session.Profile.Server, session.Profile.Context)
	}
	return ExitSuccess
}

func writeNotLoggedIn(writer io.Writer) {
	style := newTermStyle(writer)
	fmt.Fprintln(writer, "Not logged in to Doppels Cloud.")
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Login"), "doppels login")
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Local"), "doppels login --server http://127.0.0.1:4000")
}

func writeSessionSummary(writer io.Writer, identity registryclient.ActorReference, server string, ctx configstore.Context) {
	style := newTermStyle(writer)
	id := identity.ID
	if identity.DisplayName != nil {
		display := strings.TrimSpace(*identity.DisplayName)
		if display != "" && display != id {
			id = id + "  " + style.dim(display)
		}
	}
	fmt.Fprintln(writer)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Identity"), id)
	fmt.Fprintf(writer, "  %s  %s\n", style.field("Cloud"), server)
	if ctx.Valid() {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Org"), ctx.Organization)
		if ctx.Space != "" {
			fmt.Fprintf(writer, "  %s  %s\n", style.field("Space"), ctx.Space)
		}
	} else {
		fmt.Fprintf(writer, "  %s  %s\n", style.field("Org"), style.dim("none"))
	}
}

func (app *App) runContext(arguments []string) int {
	if isHelp(arguments) {
		fmt.Fprintln(app.Stdout, "Usage: doppels context [--json]")
		fmt.Fprintln(app.Stdout, "       doppels context show [--json]")
		fmt.Fprintln(app.Stdout, "Select with: doppels org use <organization>")
		fmt.Fprintln(app.Stdout, "             doppels space use <space>")
		return ExitSuccess
	}
	if len(arguments) == 0 || strings.HasPrefix(arguments[0], "-") {
		return app.contextCurrent(arguments)
	}
	switch arguments[0] {
	case "current", "show":
		return app.contextCurrent(arguments[1:])
	case "use":
		fmt.Fprintln(app.Stderr, "context use was removed; use: doppels org use <organization> and doppels space use <space>")
		return ExitContract
	default:
		fmt.Fprintf(app.Stderr, "unknown context command %q\n", arguments[0])
		return ExitContract
	}
}

func (app *App) contextCurrent(arguments []string) int {
	flags := app.flagSet("context")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "context accepts no arguments")
		return ExitContract
	}
	store, err := app.configStore()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve CLI configuration: %v\n", err)
		return ExitOperational
	}
	current, err := store.Context()
	if err != nil {
		fmt.Fprintln(app.Stderr, err)
		return ExitContract
	}
	if *jsonOutput {
		app.writeJSON(map[string]any{"kind": "CurrentContext", "context": current})
	} else {
		style := newTermStyle(app.Stdout)
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Context"), current.String())
	}
	return ExitSuccess
}
