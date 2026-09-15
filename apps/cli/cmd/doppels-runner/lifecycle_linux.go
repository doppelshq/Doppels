//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const systemdUnitName = "doppels-runner.service"

func renderSystemdUnit(opts lifecycleOptions) ([]byte, error) {
	args := opts.programArguments()
	if opts.Executable == "" || !filepath.IsAbs(opts.Executable) {
		return nil, fmt.Errorf("runner executable must be an absolute path")
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		value, err := quoteSystemd(arg)
		if err != nil {
			return nil, err
		}
		quoted[i] = value
	}
	home, err := quoteSystemd("HOME=" + opts.HomeDir)
	if err != nil {
		return nil, err
	}
	unit := `[Unit]
Description=Doppels Runner daemon
After=network.target

[Service]
Type=simple
ExecStart=` + strings.Join(quoted, " ") + `
Restart=on-failure
RestartSec=5s
Environment=` + home + `

[Install]
WantedBy=default.target
`
	return []byte(unit), nil
}

func quoteSystemd(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\n\r") {
		return "", fmt.Errorf("systemd argument contains a newline or NUL")
	}
	// Percent signs are systemd specifiers even inside quotes. A doubled
	// percent is passed through literally to the process.
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`, nil
}

func installLifecycle(opts lifecycleOptions, commands commandRunner) error {
	unit, err := renderSystemdUnit(opts)
	if err != nil {
		return err
	}
	path := systemdUnitPath(opts)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create systemd user unit directory: %w", err)
	}
	if err := writeServiceFile(path, unit, 0o644); err != nil {
		return fmt.Errorf("write systemd user unit %s: %w", path, err)
	}
	if err := runSystemctl(commands, "daemon-reload"); err != nil {
		return err
	}
	if opts.Enable {
		if err := runSystemctl(commands, "enable", systemdUnitName); err != nil {
			return err
		}
	}
	if opts.StartNow {
		if err := runSystemctl(commands, "start", systemdUnitName); err != nil {
			return err
		}
	}
	return nil
}

func uninstallLifecycle(opts lifecycleOptions, commands commandRunner) error {
	var errs []error
	if err := runSystemctl(commands, "stop", systemdUnitName); err != nil {
		errs = append(errs, err)
	}
	if err := runSystemctl(commands, "disable", systemdUnitName); err != nil {
		errs = append(errs, err)
	}
	path := systemdUnitPath(opts)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove systemd user unit %s: %w", path, err))
	}
	if err := runSystemctl(commands, "daemon-reload"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func systemdUnitPath(opts lifecycleOptions) string {
	return filepath.Join(opts.ConfigHome, "systemd", "user", systemdUnitName)
}

func runSystemctl(commands commandRunner, args ...string) error {
	fullArgs := append([]string{"--user"}, args...)
	if err := commands.Run("systemctl", fullArgs...); err != nil {
		return fmt.Errorf("systemctl --user not available or command failed (%s): %w", strings.Join(args, " "), err)
	}
	return nil
}
