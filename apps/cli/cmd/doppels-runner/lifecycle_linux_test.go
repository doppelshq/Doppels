//go:build linux

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRenderSystemdUnit(t *testing.T) {
	opts := lifecycleOptions{
		Executable: "/opt/Doppels Runner/doppels-runner",
		HomeDir:    "/home/alice",
		ConfigDir:  "/home/alice/.config/doppels",
		SocketPath: "/home/alice/Library/Application Support/doppels/runner.sock",
		Token:      "token-with-%-specifier",
	}

	unit, err := renderSystemdUnit(opts)
	if err != nil {
		t.Fatal(err)
	}
	got := string(unit)
	for _, want := range []string{
		"[Unit]\nDescription=Doppels Runner daemon\nAfter=network.target",
		"[Service]\nType=simple",
		`ExecStart="/opt/Doppels Runner/doppels-runner" "--config=/home/alice/.config/doppels" "--socket=/home/alice/Library/Application Support/doppels/runner.sock" "--token=token-with-%%-specifier"`,
		"Restart=on-failure\nRestartSec=5s",
		`Environment="HOME=/home/alice"`,
		"[Install]\nWantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit does not contain %q:\n%s", want, got)
		}
	}
}

func TestSystemdInstallAndUninstallSequence(t *testing.T) {
	configHome := t.TempDir()
	runner := &recordingCommandRunner{}
	opts := lifecycleOptions{
		Executable: "/usr/local/bin/doppels-runner",
		HomeDir:    "/home/alice",
		ConfigHome: configHome,
		ConfigDir:  "/home/alice/.config/doppels",
		Enable:     true,
		StartNow:   true,
	}

	if err := installLifecycle(opts, runner); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(configHome, "systemd", "user", systemdUnitName)
	info, err := os.Stat(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("unit mode = %#o, want 0644", got)
	}
	wantInstall := []commandCall{
		{name: "systemctl", args: []string{"--user", "daemon-reload"}},
		{name: "systemctl", args: []string{"--user", "enable", systemdUnitName}},
		{name: "systemctl", args: []string{"--user", "start", systemdUnitName}},
	}
	if !reflect.DeepEqual(runner.calls, wantInstall) {
		t.Fatalf("install calls = %#v, want %#v", runner.calls, wantInstall)
	}

	runner.calls = nil
	if err := uninstallLifecycle(opts, runner); err != nil {
		t.Fatal(err)
	}
	wantUninstall := []commandCall{
		{name: "systemctl", args: []string{"--user", "stop", systemdUnitName}},
		{name: "systemctl", args: []string{"--user", "disable", systemdUnitName}},
		{name: "systemctl", args: []string{"--user", "daemon-reload"}},
	}
	if !reflect.DeepEqual(runner.calls, wantUninstall) {
		t.Fatalf("uninstall calls = %#v, want %#v", runner.calls, wantUninstall)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("unit remains after uninstall: %v", err)
	}
	for _, call := range runner.calls {
		if call.name == "launchctl" {
			t.Fatal("Linux lifecycle called launchctl")
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
