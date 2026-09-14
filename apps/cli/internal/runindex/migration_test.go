package runindex

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenMigratesV1DatabaseAdditively(t *testing.T) {
	root := t.TempDir()
	doppelsDir := filepath.Join(root, ".doppels")
	if err := os.MkdirAll(doppelsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(doppelsDir, "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE runs (
  id TEXT PRIMARY KEY NOT NULL,
  request_id TEXT NOT NULL,
  status TEXT NOT NULL,
  source TEXT NOT NULL,
  capability TEXT NOT NULL DEFAULT '',
  recipe TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  state_dir TEXT NOT NULL,
  sync_status TEXT NOT NULL DEFAULT 'none'
);
INSERT INTO runs (id, request_id, status, source, capability, recipe, created_at, state_dir, sync_status)
VALUES ('run-v1', 'request-v1', 'succeeded', 'local', 'greet@1.0.0', '', '2026-09-14T12:00:00Z', '/state', 'none');
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	columns := tableColumns(t, idx.db, "runs")
	for _, name := range []string{"node_id", "finished_at"} {
		if !columns[name] {
			t.Fatalf("runs column %q was not added; columns = %#v", name, columns)
		}
	}
	if got, err := idx.Get("run-v1"); err != nil || got.ID != "run-v1" {
		t.Fatalf("preserved v1 row = %#v, %v", got, err)
	}
	for _, name := range []string{"capability", "idempotency_key", "run_id", "request_id", "request_fingerprint"} {
		if !tableColumns(t, idx.db, "idempotency")[name] {
			t.Fatalf("idempotency column %q missing", name)
		}
	}

	// Opening the already migrated database again must be a no-op.
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	idx, err = Open(root)
	if err != nil {
		t.Fatalf("second Open after migration: %v", err)
	}
}

func tableColumns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}
