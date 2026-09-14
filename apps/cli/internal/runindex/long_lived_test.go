package runindex

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestOpenEnablesWAL verifies the long-lived database opened by runindex uses
// WAL journal mode so the daemon can share the index with concurrent readers
// and writers (other Index instances, migration tools, smoke harnesses).
func TestOpenEnablesWAL(t *testing.T) {
	root := t.TempDir()
	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	dbPath := filepath.Join(root, ".doppels", "runs.db")
	fresh, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	var mode string
	if err := fresh.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal (daemon needs concurrent readers/writers)", mode)
	}
}

// TestOpenSetsBusyTimeout verifies the long-lived index waits at least
// busy_timeout ms before returning SQLITE_BUSY to a competing writer.
func TestOpenSetsBusyTimeout(t *testing.T) {
	root := t.TempDir()
	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	var got int
	if err := idx.db.QueryRow("PRAGMA busy_timeout").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got < 1000 {
		t.Fatalf("busy_timeout = %d ms, want >= 1000 (daemon needs resilient lock wait)", got)
	}
}

// TestOpenSerializesWritersPerIndex verifies a single Index instance serializes
// concurrent writers (one DB connection per Index) so the daemon never blocks on
// its own internal writers. Today Open leaves the *sql.DB unconstrained; this
// test pins the future long-lived contract.
func TestOpenSerializesWritersPerIndex(t *testing.T) {
	root := t.TempDir()
	idx, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := Record{
				ID:         time.Now().UTC().Format("20060102T150405.000000000") + "-" + string(rune('a'+i%26)),
				RequestID:  "req",
				Status:     "running",
				Source:     SourceLocal,
				Capability: "greet@1.0.0",
				Recipe:     "greet-shell@1.0.0",
				CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
				StateDir:   root,
				SyncStatus: SyncNone,
			}
			if err := idx.Upsert(rec); err != nil {
				t.Errorf("Upsert %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

// TestOpenAllowsConcurrentIndexInstances verifies two Open() calls against the
// same project root do not deadlock or return SQLITE_BUSY when both write, once
// WAL + busy_timeout are in place. Without WAL this test is racy on most
// filesystems; with WAL it must complete cleanly.
func TestOpenAllowsConcurrentIndexInstances(t *testing.T) {
	root := t.TempDir()

	idx1, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx1.Close()

	idx2, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()

	done := make(chan error, 2)
	go func() {
		rec := Record{
			ID: "writer-1", RequestID: "req", Status: "running", Source: SourceLocal,
			Capability: "greet@1.0.0", Recipe: "greet-shell@1.0.0",
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			StateDir:  root, SyncStatus: SyncNone,
		}
		done <- idx1.Upsert(rec)
	}()
	go func() {
		rec := Record{
			ID: "writer-2", RequestID: "req", Status: "running", Source: SourceLocal,
			Capability: "greet@1.0.0", Recipe: "greet-shell@1.0.0",
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			StateDir:  root, SyncStatus: SyncNone,
		}
		done <- idx2.Upsert(rec)
	}()

	deadline := time.After(10 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("writer: %v", err)
			}
		case <-deadline:
			t.Fatalf("writers deadlocked after 10s (WAL/busy_timeout missing?)")
		}
	}
}
