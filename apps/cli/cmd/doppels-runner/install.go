package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type lifecycleDependencies struct {
	commands   commandRunner
	executable string
	homeDir    string
	configHome string
	uid        int
}

func defaultLifecycleDependencies() (lifecycleDependencies, error) {
	executable, err := os.Executable()
	if err != nil {
		return lifecycleDependencies{}, fmt.Errorf("locate doppels-runner executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return lifecycleDependencies{}, fmt.Errorf("resolve doppels-runner executable: %w", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return lifecycleDependencies{}, fmt.Errorf("resolve user home: %w", err)
	}
	configHome, err := os.UserConfigDir()
	if err != nil {
		return lifecycleDependencies{}, fmt.Errorf("resolve user config directory: %w", err)
	}
	return lifecycleDependencies{
		commands:   execCommandRunner{},
		executable: executable,
		homeDir:    homeDir,
		configHome: configHome,
		uid:        os.Getuid(),
	}, nil
}

// executeLifecycleSubcommand parses and runs install/uninstall. handled is
// false when args belong to the daemon command instead.
func executeLifecycleSubcommand(args []string, deps lifecycleDependencies) (handled bool, err error) {
	if len(args) == 0 || (args[0] != "install" && args[0] != "uninstall") {
		return false, nil
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configDir := flags.String("config", "", "runner config dir")
	socketPath := flags.String("socket", "", "path to the IPC socket")
	token := flags.String("token", "", "runner authentication token")
	var enable, startNow *bool
	if command == "install" {
		enable = flags.Bool("enable", false, "enable automatic startup")
		startNow = flags.Bool("start-now", false, "start the runner immediately")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return true, fmt.Errorf("%s: %w", command, err)
	}
	if flags.NArg() != 0 {
		return true, fmt.Errorf("%s: unexpected argument %q", command, flags.Arg(0))
	}

	resolvedConfig := *configDir
	if resolvedConfig == "" {
		resolvedConfig = filepath.Join(deps.configHome, "doppels")
	}
	resolvedConfig, err = filepath.Abs(resolvedConfig)
	if err != nil {
		return true, fmt.Errorf("resolve config path: %w", err)
	}
	resolvedSocket := *socketPath
	if resolvedSocket != "" {
		resolvedSocket, err = filepath.Abs(resolvedSocket)
		if err != nil {
			return true, fmt.Errorf("resolve socket path: %w", err)
		}
	}
	if *token != "" {
		if _, err := validateToken(*token); err != nil {
			return true, fmt.Errorf("token: %w", err)
		}
	}
	opts := lifecycleOptions{
		Executable: deps.executable,
		HomeDir:    deps.homeDir,
		ConfigHome: deps.configHome,
		ConfigDir:  resolvedConfig,
		SocketPath: resolvedSocket,
		Token:      *token,
		UID:        deps.uid,
	}
	if command == "install" {
		opts.Enable = *enable
		opts.StartNow = *startNow
		return true, installLifecycle(opts, deps.commands)
	}
	return true, uninstallLifecycle(opts, deps.commands)
}
