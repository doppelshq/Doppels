package command

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"doppels.so/cli/internal/execution"
)

func TestWriteLocalRunSummaryStatePathOpensRunJSON(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := filepath.Join(root, ".doppels", "runs", "cea6dd2b-4add-4a18-a401-4345dfd4b467")
	abs, err := filepath.Abs(state)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	writeLocalRunSummary(&buf, execution.Result{
		Status:   "succeeded",
		StateDir: state,
		Run:      execution.RunRecord{ID: "cea6dd2b-4add-4a18-a401-4345dfd4b467"},
	}, 0)
	out := buf.String()
	want := filepath.Join(abs, "run.json")
	if !strings.Contains(out, want) {
		t.Fatalf("State must point at run.json (directory is not openable), got:\n%s", out)
	}
}

func TestFileLinkLabelIsBasename(t *testing.T) {
	t.Parallel()
	got := fileLinkLabel("/Users/turbo/projects/opendoppel/examples/acme/support/.doppels/runs/9e0a9a3a-bfbe-4b7b-9a0c-9f6dcb2a7cb0/run.json")
	if got != "run.json" {
		t.Fatalf("fileLinkLabel() = %q, want run.json", got)
	}
}

func TestShareTimelineHeaderOmitsCatalogAndFullID(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	timeline := &runTimeline{
		writer:      &buf,
		style:       newTermStyle(&buf),
		hideCatalog: true,
		capName:     "escalate-ticket",
		recipe:      "escalate-ticket-zendesk",
	}
	timeline.writeHeader("9e0a9a3a-bfbe-4b7b-9a0c-9f6dcb2a7cb0")
	out := buf.String()
	if !strings.Contains(out, "9e0a9a3a") {
		t.Fatalf("missing short Run id:\n%s", out)
	}
	for _, forbidden := range []string{"Cap", "Recipe", "escalate-ticket", "9e0a9a3a-bfbe"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("share timeline still repeats %q:\n%s", forbidden, out)
		}
	}
}

func TestPrefixLinesIndentsStepOutput(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := prefixLines(&buf, "    ")
	if _, err := w.Write([]byte("mock escalate-ticket\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "    mock escalate-ticket\n" {
		t.Fatalf("prefixLines = %q", got)
	}
}
