package command

import (
	"strings"
	"testing"
)

func TestSpacesInitRedirectsToInit(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := testApp(root)
	if code := app.Run([]string{"spaces", "init"}); code != ExitContract {
		t.Fatalf("exit = %d, want %d", code, ExitContract)
	}
	if !strings.Contains(stderr.String(), "doppels init") {
		t.Fatalf("expected redirect hint in stderr: %q", stderr.String())
	}
}
