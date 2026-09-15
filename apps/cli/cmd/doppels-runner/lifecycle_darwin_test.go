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
