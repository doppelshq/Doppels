//go:build darwin

package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	launchdLabel     = "dev.doppels.runner"
	launchdPlistName = "doppels.so.doppels-runner.plist"
)

func renderLaunchdPlist(opts lifecycleOptions) ([]byte, error) {
	if opts.Executable == "" || !filepath.IsAbs(opts.Executable) {
		return nil, fmt.Errorf("runner executable must be an absolute path")
	}
	args := opts.programArguments()
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("launchd argument contains a NUL")
		}
	}
	var arguments strings.Builder
	for _, arg := range args {
		arguments.WriteString("      <string>")
		arguments.WriteString(escapeXML(arg))
		arguments.WriteString("</string>\n")
	}
	runAtLoad := ""
	if opts.Enable {
		runAtLoad = "    <key>RunAtLoad</key>\n    <true/>\n"
	}
	logsDir := filepath.Join(opts.HomeDir, "Library", "Logs")
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>` + launchdLabel + `</string>
    <key>ProgramArguments</key>
    <array>
` + arguments.String() + `    </array>
` + runAtLoad + `    <key>KeepAlive</key>
    <dict>
      <key>SuccessfulExit</key>
      <false/>
      <key>NetworkState</key>
      <true/>
    </dict>
    <key>StandardOutPath</key>
    <string>` + escapeXML(filepath.Join(logsDir, "doppels-runner.out")) + `</string>
    <key>StandardErrorPath</key>
    <string>` + escapeXML(filepath.Join(logsDir, "doppels-runner.err")) + `</string>
    <key>EnvironmentVariables</key>
    <dict>
      <key>HOME</key>
      <string>` + escapeXML(opts.HomeDir) + `</string>
    </dict>
  </dict>
</plist>
`
	return []byte(plist), nil
}

func escapeXML(value string) string {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

func installLifecycle(opts lifecycleOptions, commands commandRunner) error {
	plist, err := renderLaunchdPlist(opts)
	if err != nil {
		return err
	}
	logsDir := filepath.Join(opts.HomeDir, "Library", "Logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("create runner log directory: %w", err)
	}
	path := launchdPlistPath(opts)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	if err := writeServiceFile(path, plist, 0o644); err != nil {
		return fmt.Errorf("write launchd plist %s: %w", path, err)
	}
	domain := launchdDomain(opts)
	if opts.Enable {
		if err := commands.Run("launchctl", "bootstrap", domain, path); err != nil {
			// `bootstrap` arrived in macOS 10.10. Keep the documented load -w
			// fallback so the same binary remains usable on older installations.
			if fallbackErr := commands.Run("launchctl", "load", "-w", path); fallbackErr != nil {
				return fmt.Errorf("launchctl not found or bootstrap failed: %v (load -w fallback: %w)", err, fallbackErr)
			}
		}
	}
	if opts.StartNow {
		target := domain + "/" + launchdLabel
		if err := commands.Run("launchctl", "kickstart", "-k", target); err != nil {
			return fmt.Errorf("launchctl kickstart %s: %w", target, err)
		}
	}
	return nil
}

func uninstallLifecycle(opts lifecycleOptions, commands commandRunner) error {
	var errs []error
	target := launchdDomain(opts) + "/" + launchdLabel
	if err := commands.Run("launchctl", "bootout", target); err != nil {
		errs = append(errs, fmt.Errorf("launchctl not found or bootout failed for %s: %w", target, err))
	}
	path := launchdPlistPath(opts)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove launchd plist %s: %w", path, err))
	}
	return errors.Join(errs...)
}

func launchdPlistPath(opts lifecycleOptions) string {
	return filepath.Join(opts.HomeDir, "Library", "LaunchAgents", launchdPlistName)
}

func launchdDomain(opts lifecycleOptions) string {
	return "gui/" + strconv.Itoa(opts.UID)
}
