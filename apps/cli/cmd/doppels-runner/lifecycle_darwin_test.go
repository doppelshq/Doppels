//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRenderLaunchdPlist(t *testing.T) {
	opts := lifecycleOptions{
		Executable: "/Applications/Doppels & Runner/doppels-runner",
		HomeDir:    "/Users/alice",
		ConfigDir:  "/Users/alice/Library/Application Support/doppels",
		SocketPath: "/Users/alice/Library/Application Support/doppels/runner.sock",
		Token:      "token<secret>",
		Enable:     true,
	}

	plist, err := renderLaunchdPlist(opts)
	if err != nil {
		t.Fatal(err)
	}
	got := string(plist)
	for _, want := range []string{
		"<key>Label</key>\n    <string>dev.doppels.runner</string>",
		"<string>/Applications/Doppels &amp; Runner/doppels-runner</string>",
		"<string>--config=/Users/alice/Library/Application Support/doppels</string>",
		"<string>--socket=/Users/alice/Library/Application Support/doppels/runner.sock</string>",
		"<string>--token=token&lt;secret&gt;</string>",
		"<key>RunAtLoad</key>\n    <true/>",
		"<key>SuccessfulExit</key>\n      <false/>",
		"<key>NetworkState</key>\n      <true/>",
		"<string>/Users/alice/Library/Logs/doppels-runner.out</string>",
		"<string>/Users/alice/Library/Logs/doppels-runner.err</string>",
		"<key>HOME</key>\n      <string>/Users/alice</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist does not contain %q:\n%s", want, got)
		}
	}
}

func TestRenderLaunchdPlistOmitsRunAtLoadWithoutEnable(t *testing.T) {
	plist, err := renderLaunchdPlist(lifecycleOptions{
		Executable: "/usr/local/bin/doppels-runner",
		HomeDir:    "/Users/alice",
		ConfigDir:  "/Users/alice/.config/doppels",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plist), "RunAtLoad") {
		t.Fatalf("RunAtLoad present without --enable:\n%s", plist)
	}
}

func TestLaunchdInstallAndUninstallSequence(t *testing.T) {
	home := t.TempDir()
	runner := &recordingCommandRunner{}
	opts := lifecycleOptions{
		Executable: "/usr/local/bin/doppels-runner",
		HomeDir:    home,
		ConfigDir:  filepath.Join(home, ".config", "doppels"),
		UID:        501,
		Enable:     true,
		StartNow:   true,
	}

	if err := installLifecycle(opts, runner); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdPlistName)
	info, err := os.Stat(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("plist mode = %#o, want 0644", got)
	}
	wantInstall := []commandCall{
		{name: "launchctl", args: []string{"bootstrap", "gui/501", plistPath}},
		{name: "launchctl", args: []string{"kickstart", "-k", "gui/501/dev.doppels.runner"}},
	}
	if !reflect.DeepEqual(runner.calls, wantInstall) {
		t.Fatalf("install calls = %#v, want %#v", runner.calls, wantInstall)
	}

	runner.calls = nil
	if err := uninstallLifecycle(opts, runner); err != nil {
		t.Fatal(err)
	}
	wantUninstall := []commandCall{
		{name: "launchctl", args: []string{"bootout", "gui/501/dev.doppels.runner"}},
	}
	if !reflect.DeepEqual(runner.calls, wantUninstall) {
		t.Fatalf("uninstall calls = %#v, want %#v", runner.calls, wantUninstall)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("plist remains after uninstall: %v", err)
	}
	for _, call := range runner.calls {
		if call.name == "systemctl" {
			t.Fatal("macOS lifecycle called systemctl")
		}
	}
}

type commandCall struct {
	name string
	args []string
}

type recordingCommandRunner struct {
	calls []commandCall
	err   error
}

func (r *recordingCommandRunner) Run(name string, args ...string) error {
	r.calls = append(r.calls, commandCall{name: name, args: append([]string(nil), args...)})
	return r.err
}
