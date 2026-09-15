//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallSubcommandResolvesAndPersistsDaemonFlags(t *testing.T) {
	root := t.TempDir()
	runner := &recordingCommandRunner{}
	deps := lifecycleDependencies{
		commands:   runner,
		executable: filepath.Join(root, "bin", "doppels-runner"),
		homeDir:    filepath.Join(root, "home"),
		configHome: filepath.Join(root, "xdg"),
		uid:        1001,
	}
	token := strings.Repeat("a", 64)
	configDir := filepath.Join(root, "runner config")
	socketPath := filepath.Join(root, "runner socket.sock")

	handled, err := executeLifecycleSubcommand([]string{
		"install",
		"--enable",
		"--start-now",
		"--config", configDir,
		"--socket", socketPath,
		"--token", token,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("install subcommand was not handled")
	}
	unitPath := filepath.Join(deps.configHome, "systemd", "user", systemdUnitName)
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"--config=` + configDir + `"`,
		`"--socket=` + socketPath + `"`,
		`"--token=` + token + `"`,
	} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit does not contain %q:\n%s", want, unit)
		}
	}
	if len(runner.calls) != 3 {
		t.Fatalf("commands = %#v, want daemon-reload, enable, start", runner.calls)
	}
}

func TestUninstallSubcommandAcceptsDaemonFlags(t *testing.T) {
	root := t.TempDir()
	runner := &recordingCommandRunner{}
	deps := lifecycleDependencies{
		commands:   runner,
		executable: filepath.Join(root, "doppels-runner"),
		homeDir:    filepath.Join(root, "home"),
		configHome: filepath.Join(root, "xdg"),
		uid:        1001,
	}

	handled, err := executeLifecycleSubcommand([]string{
		"uninstall",
		"--config", filepath.Join(root, "config"),
		"--socket", filepath.Join(root, "socket"),
		"--token", strings.Repeat("b", 64),
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("uninstall subcommand was not handled")
	}
	if len(runner.calls) != 3 {
		t.Fatalf("commands = %#v, want stop, disable, daemon-reload", runner.calls)
	}
}

func TestLifecycleSubcommandRejectsUnexpectedArguments(t *testing.T) {
	handled, err := executeLifecycleSubcommand([]string{"install", "surprise"}, lifecycleDependencies{})
	if !handled {
		t.Fatal("install subcommand was not handled")
	}
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("error = %v, want actionable unexpected argument error", err)
	}
}

func TestNonLifecycleCommandIsNotHandled(t *testing.T) {
	handled, err := executeLifecycleSubcommand([]string{"--config", t.TempDir()}, lifecycleDependencies{})
	if err != nil || handled {
		t.Fatalf("handled, err = %v, %v; want false, nil", handled, err)
	}
}

func TestDefaultLifecycleDependenciesHonorRunnerConfigOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "runner-config")
	t.Setenv("DOPPELS_RUNNER_CONFIG", want)
	deps, err := defaultLifecycleDependencies()
	if err != nil {
		t.Fatal(err)
	}
	if deps.configDir != want {
		t.Fatalf("default config dir = %q, want override %q", deps.configDir, want)
	}
}
