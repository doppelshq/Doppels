package command

import (
	"strings"
	"testing"

	"doppels.so/cli/internal/version"
)

func TestVersionFlag(t *testing.T) {
	previous := version.Version
	t.Cleanup(func() { version.Version = previous })
	version.Version = "9.9.9-test"

	for _, flag := range []string{"--version", "-v", "version"} {
		t.Run(flag, func(t *testing.T) {
			root := t.TempDir()
			app, stdout, stderr := testApp(root)
			if code := app.Run([]string{flag}); code != ExitSuccess {
				t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
			}
			got := strings.TrimSpace(stdout.String())
			if got != "doppels 9.9.9-test" {
				t.Fatalf("stdout = %q", got)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %s", stderr.String())
			}
		})
	}
}
