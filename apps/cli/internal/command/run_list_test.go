package command

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/runindex"
)

func seedIndexedRuns(t *testing.T, root string, n int) []string {
	t.Helper()
	_ = writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	idx, err := runindex.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ids := make([]string, 0, n)
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012d", i+1, i+1)
		ids = append(ids, id)
		created := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		if err := idx.Upsert(runindex.Record{
			ID: id, RequestID: "req-" + id, Status: "succeeded", Source: runindex.SourceLocal,
			Capability: "greet@1.0.0", Recipe: "greet-shell@1.0.0", CreatedAt: created,
			StateDir: root + "/.doppels/runs/" + id, SyncStatus: runindex.SyncNone,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func TestRunsListLimitsHumanOutput(t *testing.T) {
	root := t.TempDir()
	ids := seedIndexedRuns(t, root, 25)
	app, stdout, stderr := testApp(root)

	if code := app.Run([]string{"runs", "list"}); code != ExitSuccess {
		t.Fatalf("list exit = %d, stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "showing 20 of 25") {
		t.Fatalf("expected truncation footer, got %s", out)
	}
	if strings.Contains(out, "SOURCE") {
		t.Fatalf("SOURCE column should hide when all local: %s", out)
	}
	newestShort := shortRunID(ids[24])
	if !strings.Contains(out, newestShort) {
		t.Fatalf("expected newest short id %s in %s", newestShort, out)
	}
	if strings.Count(out, "succeeded") < 20 {
		t.Fatalf("expected ~20 rows, got %s", out)
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"runs", "list", "--limit", "5"}); code != ExitSuccess {
		t.Fatalf("limit exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "showing 5 of 25") {
		t.Fatalf("stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"runs", "list", "--all"}); code != ExitSuccess {
		t.Fatalf("all exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "showing 20 of") {
		t.Fatalf("unexpected truncate footer with --all: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "showing 25 of 25") {
		t.Fatalf("expected full count footer, got %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"runs", "list", "--json"}); code != ExitSuccess {
		t.Fatalf("json exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), ids[0]) || !strings.Contains(stdout.String(), ids[24]) {
		t.Fatalf("json should include all full ids: %s", stdout.String())
	}
}

func TestResolveRunIDPrefix(t *testing.T) {
	root := t.TempDir()
	ids := seedIndexedRuns(t, root, 2)
	got, err := resolveRunID(root, shortRunID(ids[1]))
	if err != nil {
		t.Fatal(err)
	}
	if got != ids[1] {
		t.Fatalf("got %s want %s", got, ids[1])
	}
	_, err = resolveRunID(root, "0000000")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous, got %v", err)
	}
}

func TestRunsShowAcceptsShortID(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "capabilities", "greet.yaml", runCapabilityFixture)
	writeManifest(t, root, "recipes", "greet.yaml", runRecipeFixture)
	app, stdout, stderr := testApp(root)
	app.Environment = []string{"PATH=" + os.Getenv("PATH"), "DOPPELS_IDENTITY=tester"}
	app.Hostname = func() (string, error) { return "test-node", nil }
	if code := app.Run([]string{"run", "capability/greet", "--input", "name=Ada", "--json"}); code != ExitSuccess {
		t.Fatalf("run exit = %d, stderr = %s", code, stderr.String())
	}
	var executed struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &executed); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"runs", "show", shortRunID(executed.Run.ID)}); code != ExitSuccess {
		t.Fatalf("show short exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), executed.Run.ID) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}
