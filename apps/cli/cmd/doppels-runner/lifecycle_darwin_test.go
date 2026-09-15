//go:build darwin

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

func TestLaunchdInstallFlagMatrix(t *testing.T) {
	tests := []struct {
		name     string
		enable   bool
		startNow bool
		want     func(string) []commandCall
	}{
		{name: "install", want: func(string) []commandCall { return nil }},
		{
			name:   "enable",
			enable: true,
			want: func(path string) []commandCall {
				return []commandCall{{name: "launchctl", args: []string{"bootstrap", "gui/501", path}}}
			},
		},
		{
			name:     "start now",
			startNow: true,
			want: func(string) []commandCall {
				return []commandCall{{name: "launchctl", args: []string{"kickstart", "-k", "gui/501/dev.doppels.runner"}}}
			},
		},
		{
			name:     "enable and start now",
			enable:   true,
			startNow: true,
			want: func(path string) []commandCall {
				return []commandCall{
					{name: "launchctl", args: []string{"bootstrap", "gui/501", path}},
					{name: "launchctl", args: []string{"kickstart", "-k", "gui/501/dev.doppels.runner"}},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			runner := &recordingCommandRunner{}
			opts := lifecycleOptions{
				Executable: "/usr/local/bin/doppels-runner",
				HomeDir:    home,
				ConfigDir:  filepath.Join(home, ".config", "doppels"),
				UID:        501,
				Enable:     tt.enable,
				StartNow:   tt.startNow,
			}
			plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdPlistName)
			if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plistPath, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := installLifecycle(opts, runner); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(plistPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o644 {
				t.Fatalf("plist mode = %#o, want 0644", got)
			}
			plist, err := os.ReadFile(plistPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(plist), "RunAtLoad"); got != tt.enable {
				t.Fatalf("RunAtLoad present = %t, want %t:\n%s", got, tt.enable, plist)
			}
			if want := tt.want(plistPath); !reflect.DeepEqual(runner.calls, want) {
				t.Fatalf("install calls = %#v, want %#v", runner.calls, want)
			}
		})
	}
}

func TestLaunchdUninstallSequence(t *testing.T) {
	home := t.TempDir()
	runner := &recordingCommandRunner{}
	opts := lifecycleOptions{HomeDir: home, UID: 501}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdPlistName)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := uninstallLifecycle(opts, runner); err != nil {
		t.Fatal(err)
	}
	want := []commandCall{{name: "launchctl", args: []string{"bootout", "gui/501/dev.doppels.runner"}}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("uninstall calls = %#v, want %#v", runner.calls, want)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("plist remains after uninstall: %v", err)
	}
}

func TestConcurrentLaunchdInstallsNeverExposeTornPlist(t *testing.T) {
	home := t.TempDir()
	base := lifecycleOptions{
		Executable: "/usr/local/bin/doppels-runner",
		HomeDir:    home,
		UID:        501,
	}
	optsA := base
	optsA.ConfigDir = "/tmp/" + strings.Repeat("a", 4<<20)
	optsA.Enable = true
	optsB := base
	optsB.ConfigDir = "/tmp/" + strings.Repeat("b", 2<<20)
	optsB.StartNow = true
	wantA, err := renderLaunchdPlist(optsA)
	if err != nil {
		t.Fatal(err)
	}
	wantB, err := renderLaunchdPlist(optsB)
	if err != nil {
		t.Fatal(err)
	}

	path := launchdPlistPath(base)
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
		t.Fatalf("final plist is torn: got %d bytes, want %d or %d", len(got), len(wantA), len(wantB))
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
