//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

func TestRenderSystemdUnitEscapesVariableExpansionInExecValues(t *testing.T) {
	opts := lifecycleOptions{
		Executable: "/opt/${UNSET}/doppels-runner",
		HomeDir:    "/home/alice",
		ConfigDir:  "/config/${UNSET}",
		SocketPath: "/run/${UNSET}/runner.sock",
		Token:      "token-${UNSET}",
	}

	unit, err := renderSystemdUnit(opts)
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart="/opt/$${UNSET}/doppels-runner" "--config=/config/$${UNSET}" "--socket=/run/$${UNSET}/runner.sock" "--token=token-$${UNSET}"`
	if !strings.Contains(string(unit), want) {
		t.Fatalf("unit does not contain literal variable references %q:\n%s", want, unit)
	}
}

func TestEscapeSystemdExecValue(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  string
	}{
		{value: `${UNSET}`, want: `$${UNSET}`},
		{value: `$$`, want: `$$$$`},
		{value: `$NAME`, want: `$$$NAME`},
		{value: `trailing$`, want: `trailing$$$`},
	} {
		if got := escapeSystemdExecValue(tt.value); got != tt.want {
			t.Errorf("escapeSystemdExecValue(%q) = %q, want %q", tt.value, got, tt.want)
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
	unitPath := filepath.Join(configHome, "systemd", "user", systemdUnitName)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := installLifecycle(opts, runner); err != nil {
		t.Fatal(err)
	}
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

func TestSystemdInstallFlagMatrix(t *testing.T) {
	tests := []struct {
		name     string
		enable   bool
		startNow bool
		want     []commandCall
	}{
		{
			name: "install",
			want: []commandCall{{name: "systemctl", args: []string{"--user", "daemon-reload"}}},
		},
		{
			name:   "enable",
			enable: true,
			want: []commandCall{
				{name: "systemctl", args: []string{"--user", "daemon-reload"}},
				{name: "systemctl", args: []string{"--user", "enable", systemdUnitName}},
			},
		},
		{
			name:     "start now",
			startNow: true,
			want: []commandCall{
				{name: "systemctl", args: []string{"--user", "daemon-reload"}},
				{name: "systemctl", args: []string{"--user", "start", systemdUnitName}},
			},
		},
		{
			name:     "enable and start now",
			enable:   true,
			startNow: true,
			want: []commandCall{
				{name: "systemctl", args: []string{"--user", "daemon-reload"}},
				{name: "systemctl", args: []string{"--user", "enable", systemdUnitName}},
				{name: "systemctl", args: []string{"--user", "start", systemdUnitName}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configHome := t.TempDir()
			runner := &recordingCommandRunner{}
			opts := lifecycleOptions{
				Executable: "/usr/local/bin/doppels-runner",
				HomeDir:    "/home/alice",
				ConfigHome: configHome,
				ConfigDir:  "/home/alice/.config/doppels",
				Enable:     tt.enable,
				StartNow:   tt.startNow,
			}
			if err := installLifecycle(opts, runner); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(systemdUnitPath(opts)); err != nil {
				t.Fatalf("unit was not written: %v", err)
			}
			if !reflect.DeepEqual(runner.calls, tt.want) {
				t.Fatalf("install calls = %#v, want %#v", runner.calls, tt.want)
			}
		})
	}
}

func TestConcurrentSystemdInstallsNeverExposeTornUnit(t *testing.T) {
	configHome := t.TempDir()
	base := lifecycleOptions{
		Executable: "/usr/local/bin/doppels-runner",
		HomeDir:    "/home/alice",
		ConfigHome: configHome,
	}
	optsA := base
	optsA.ConfigDir = "/tmp/" + strings.Repeat("a", 4<<20)
	optsA.Enable = true
	optsB := base
	optsB.ConfigDir = "/tmp/" + strings.Repeat("b", 2<<20)
	optsB.StartNow = true
	wantA, err := renderSystemdUnit(optsA)
	if err != nil {
		t.Fatal(err)
	}
	wantB, err := renderSystemdUnit(optsB)
	if err != nil {
		t.Fatal(err)
	}

	path := systemdUnitPath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, wantA, 0o644); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	readerReady := make(chan struct{})
	stopReader := make(chan struct{})
	readerDone := make(chan struct{})
	unexpected := make(chan string, 1)
	go observeWholeServiceFiles(path, wantA, wantB, readerReady, stopReader, readerDone, unexpected)
	<-readerReady

	errCh := make(chan error, 2)
	var writers sync.WaitGroup
	for _, opts := range []lifecycleOptions{optsA, optsB} {
		opts := opts
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			errCh <- installLifecycle(opts, successfulCommandRunner{})
		}()
	}
	close(start)
	writers.Wait()
	close(stopReader)
	<-readerDone
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-unexpected:
		t.Fatal(got)
	default:
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantA) && !bytes.Equal(got, wantB) {
		t.Fatalf("final unit is torn: got %d bytes, want %d or %d", len(got), len(wantA), len(wantB))
	}
}

func observeWholeServiceFiles(path string, wantA, wantB []byte, ready chan<- struct{}, stop <-chan struct{}, done chan<- struct{}, unexpected chan<- string) {
	defer close(done)
	close(ready)
	for {
		select {
		case <-stop:
			return
		default:
		}
		got, err := os.ReadFile(path)
		if err != nil {
			select {
			case unexpected <- "read service file during install: " + err.Error():
			default:
			}
			return
		}
		if !bytes.Equal(got, wantA) && !bytes.Equal(got, wantB) {
			select {
			case unexpected <- "observed torn service file with " + fmt.Sprint(len(got)) + " bytes":
			default:
			}
			return
		}
	}
}

type successfulCommandRunner struct{}

func (successfulCommandRunner) Run(string, ...string) error { return nil }

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
