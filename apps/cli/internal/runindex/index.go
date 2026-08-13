// Package runindex maintains a SQLite index of local Runs. Artifacts and logs
// stay on disk under .doppels/runs/<id>/; this package only stores queryable
// metadata and an outbox for future control-plane sync.
package runindex

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	SourceLocal = "local"
	SourceCloud = "cloud"

	SyncPending = "pending"
	SyncSynced  = "synced"
	SyncNone    = "none"
)

type Record struct {
	ID         string `json:"id"`
	RequestID  string `json:"requestId"`
	Status     string `json:"status"`
	Source     string `json:"source"`
	Capability string `json:"capability"`
	Recipe     string `json:"recipe,omitempty"`
	CreatedAt  string `json:"createdAt"`
	StateDir   string `json:"stateDir"`
	SyncStatus string `json:"syncStatus,omitempty"`
}

type OutboxItem struct {
	ID        int64
	RunID     string
	Payload   string
	CreatedAt string
	Attempts  int
}

type Index struct {
	db   *sql.DB
	root string
}

func Open(projectRoot string) (*Index, error) {
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	dir := filepath.Join(root, ".doppels")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create .doppels: %w", err)
	}
	dbPath := filepath.Join(dir, "runs.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open runs.db: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, err
	}
	idx := &Index{db: db, root: root}
	if err := idx.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := idx.Backfill(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("backfill run index: %w", err)
	}
	return idx, nil
}

func (idx *Index) Close() error {
	if idx == nil || idx.db == nil {
		return nil
	}
	return idx.db.Close()
}

func (idx *Index) migrate() error {
	_, err := idx.db.Exec(`
CREATE TABLE IF NOT EXISTS runs (
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
CREATE TABLE IF NOT EXISTS outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS runs_created_at ON runs(created_at DESC);
`)
	return err
}

func (idx *Index) Upsert(record Record) error {
	if record.Source == "" {
		record.Source = SourceLocal
	}
	if record.SyncStatus == "" {
		record.SyncStatus = SyncNone
	}
	_, err := idx.db.Exec(`
INSERT INTO runs (id, request_id, status, source, capability, recipe, created_at, state_dir, sync_status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  request_id = excluded.request_id,
  status = excluded.status,
  source = excluded.source,
  capability = excluded.capability,
  recipe = excluded.recipe,
  created_at = excluded.created_at,
  state_dir = excluded.state_dir,
  sync_status = excluded.sync_status
`, record.ID, record.RequestID, record.Status, record.Source, record.Capability, record.Recipe,
		record.CreatedAt, record.StateDir, record.SyncStatus)
	return err
}

func (idx *Index) List() ([]Record, error) {
	rows, err := idx.db.Query(`
SELECT id, request_id, status, source, capability, recipe, created_at, state_dir, sync_status
FROM runs
ORDER BY created_at DESC, id ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Record
	for rows.Next() {
		var item Record
		if err := rows.Scan(&item.ID, &item.RequestID, &item.Status, &item.Source, &item.Capability,
			&item.Recipe, &item.CreatedAt, &item.StateDir, &item.SyncStatus); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if result == nil {
		result = []Record{}
	}
	return result, rows.Err()
}

func (idx *Index) Get(id string) (Record, error) {
	var item Record
	err := idx.db.QueryRow(`
SELECT id, request_id, status, source, capability, recipe, created_at, state_dir, sync_status
FROM runs WHERE id = ?
`, id).Scan(&item.ID, &item.RequestID, &item.Status, &item.Source, &item.Capability,
		&item.Recipe, &item.CreatedAt, &item.StateDir, &item.SyncStatus)
	return item, err
}

func (idx *Index) EnqueueOutbox(runID string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = idx.db.Exec(`
INSERT INTO outbox (run_id, payload_json, created_at, attempts)
VALUES (?, ?, ?, 0)
`, runID, string(data), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	_, err = idx.db.Exec(`UPDATE runs SET sync_status = ? WHERE id = ?`, SyncPending, runID)
	return err
}

func (idx *Index) ListOutbox() ([]OutboxItem, error) {
	rows, err := idx.db.Query(`
SELECT id, run_id, payload_json, created_at, attempts FROM outbox ORDER BY id ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []OutboxItem
	for rows.Next() {
		var item OutboxItem
		if err := rows.Scan(&item.ID, &item.RunID, &item.Payload, &item.CreatedAt, &item.Attempts); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if result == nil {
		result = []OutboxItem{}
	}
	return result, rows.Err()
}

func (idx *Index) AckOutbox(id int64, runID string) error {
	_, err := idx.db.Exec(`DELETE FROM outbox WHERE id = ?`, id)
	if err != nil {
		return err
	}
	_, err = idx.db.Exec(`UPDATE runs SET sync_status = ? WHERE id = ?`, SyncSynced, runID)
	return err
}

func (idx *Index) BumpOutboxAttempt(id int64) error {
	_, err := idx.db.Exec(`UPDATE outbox SET attempts = attempts + 1 WHERE id = ?`, id)
	return err
}
