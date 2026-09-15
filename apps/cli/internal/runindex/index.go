// Package runindex maintains a SQLite index of local Runs. Artifacts and logs
// stay on disk under .doppels/runs/<id>/; this package only stores queryable
// metadata and an outbox for future control-plane sync.
package runindex

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

var (
	ErrIdempotencyConflict = errors.New("idempotency key was already used with different inputs")
	ErrInvalidCursor       = errors.New("invalid runs cursor")
	ErrInvalidLimit        = errors.New("invalid runs limit")
)

const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

type Record struct {
	ID         string `json:"id"`
	RequestID  string `json:"requestId"`
	Status     string `json:"status"`
	Source     string `json:"source"`
	Capability string `json:"capability"`
	Recipe     string `json:"recipe,omitempty"`
	NodeID     string `json:"nodeId,omitempty"`
	CreatedAt  string `json:"createdAt"`
	FinishedAt string `json:"finishedAt,omitempty"`
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

type IdempotencyRecord struct {
	Capability  string
	Key         string
	RunID       string
	RequestID   string
	Fingerprint string
	RequestJSON string
	RunJSON     string
}

type ReservationEvidence struct {
	RequestJSON string
	RunJSON     string
}

type ListQuery struct {
	Capability string
	Status     string
	Limit      int
	Cursor     string
}

type Page struct {
	Records    []Record
	NextCursor string
}

type listCursor struct {
	Version    int    `json:"v"`
	CreatedAt  string `json:"createdAt"`
	ID         string `json:"id"`
	Capability string `json:"capability"`
	Status     string `json:"status"`
}

type Index struct {
	db   *sql.DB
	root string

	// testBeforeEnrichUpdate is a test-only scheduling seam used to pin the
	// interleaving between Backfill's disk read and its conditional UPDATE.
	testBeforeEnrichUpdate func()
	// testAfterCommitTerminalLock is a test-only scheduling seam invoked
	// once CommitTerminal has forced acquisition of the database's single
	// WAL write lock, immediately before it reads current state to decide
	// what to write. Used to pin the interleaving between CommitTerminal and
	// a concurrent AckOutbox from a different *Index (different connection).
	testAfterCommitTerminalLock func()
}

// busyTimeoutMS is the SQLITE_BUSY wait applied to every connection in the
// pool. Long-lived daemon writers and migration tools may race briefly when
// both touch the index; waiting up to this many milliseconds before returning
// SQLITE_BUSY lets concurrent writers finish cleanly without holding a mutex
// in Go. Tests in long_lived_test.go pin this contract.
const busyTimeoutMS = 5000

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
	dsn := (&url.URL{Scheme: "file", Path: dbPath}).String() + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open runs.db: %w", err)
	}
	// Long-lived daemon contract: a single writer connection per *Index
	// instance. Concurrent writers inside the same Index are serialized by the
	// database engine; concurrent writers across Index instances (daemon +
	// CLI smoke) must rely on WAL + busy_timeout.
	db.SetMaxOpenConns(1)
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
	var version int
	if err := idx.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 4 {
		return nil
	}
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
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
DELETE FROM outbox
WHERE id NOT IN (SELECT MIN(id) FROM outbox GROUP BY run_id);
CREATE UNIQUE INDEX IF NOT EXISTS outbox_run_id ON outbox(run_id);
`); err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "node_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "finished_at", definition: "TEXT NOT NULL DEFAULT ''"},
	} {
		exists, err := columnExists(tx, "runs", column.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.Exec("ALTER TABLE runs ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS idempotency (
  capability TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  run_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  request_fingerprint TEXT NOT NULL,
  request_json TEXT NOT NULL DEFAULT '',
  run_json TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (capability, idempotency_key)
);
CREATE INDEX IF NOT EXISTS runs_created_at ON runs(created_at DESC, id ASC);
CREATE INDEX IF NOT EXISTS runs_capability_created_at ON runs(capability, created_at DESC, id ASC);
CREATE INDEX IF NOT EXISTS runs_status_created_at ON runs(status, created_at DESC, id ASC);
`); err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "request_json", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "run_json", definition: "TEXT NOT NULL DEFAULT ''"},
	} {
		exists, err := columnExists(tx, "idempotency", column.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.Exec("ALTER TABLE idempotency ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`PRAGMA user_version = 4;`); err != nil {
		return err
	}
	return tx.Commit()
}

type columnQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func columnExists(db columnQuerier, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (idx *Index) Upsert(record Record) error {
	if record.Source == "" {
		record.Source = SourceLocal
	}
	if record.SyncStatus == "" {
		record.SyncStatus = SyncNone
	}
	_, err := idx.db.Exec(`
INSERT INTO runs (id, request_id, status, source, capability, recipe, node_id, created_at, finished_at, state_dir, sync_status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  request_id = excluded.request_id,
  status = excluded.status,
  source = excluded.source,
  capability = excluded.capability,
  recipe = excluded.recipe,
  node_id = excluded.node_id,
  finished_at = excluded.finished_at,
  state_dir = excluded.state_dir,
  sync_status = excluded.sync_status
`, record.ID, record.RequestID, record.Status, record.Source, record.Capability, record.Recipe,
		record.NodeID, record.CreatedAt, record.FinishedAt, record.StateDir, record.SyncStatus)
	return err
}

// ReserveStart atomically reserves an idempotency key and inserts the
// corresponding initial Run row. A retry with the same fingerprint returns
// the original identifiers without replacing the Run; a different
// fingerprint returns ErrIdempotencyConflict. Keeping both writes in one
// transaction prevents an idempotency reservation without an indexed Run.
func (idx *Index) ReserveStart(record Record, key, fingerprint string, evidence ReservationEvidence) (IdempotencyRecord, bool, error) {
	if record.Source == "" {
		record.Source = SourceLocal
	}
	if record.SyncStatus == "" {
		record.SyncStatus = SyncNone
	}
	tx, err := idx.db.Begin()
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`
INSERT INTO idempotency (capability, idempotency_key, run_id, request_id, request_fingerprint, request_json, run_json)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(capability, idempotency_key) DO NOTHING
`, record.Capability, key, record.ID, record.RequestID, fingerprint, evidence.RequestJSON, evidence.RunJSON)
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	reservation := IdempotencyRecord{Capability: record.Capability, Key: key}
	if err := tx.QueryRow(`
SELECT run_id, request_id, request_fingerprint, request_json, run_json
FROM idempotency WHERE capability = ? AND idempotency_key = ?
`, record.Capability, key).Scan(&reservation.RunID, &reservation.RequestID, &reservation.Fingerprint, &reservation.RequestJSON, &reservation.RunJSON); err != nil {
		return IdempotencyRecord{}, false, err
	}
	if reservation.Fingerprint != fingerprint {
		return IdempotencyRecord{}, false, ErrIdempotencyConflict
	}
	created := rows == 1
	if created {
		if _, err := tx.Exec(`
INSERT INTO runs (id, request_id, status, source, capability, recipe, node_id, created_at, finished_at, state_dir, sync_status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, record.ID, record.RequestID, record.Status, record.Source, record.Capability, record.Recipe,
			record.NodeID, record.CreatedAt, record.FinishedAt, record.StateDir, record.SyncStatus); err != nil {
			return IdempotencyRecord{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return IdempotencyRecord{}, false, err
	}
	return reservation, created, nil
}

func (idx *Index) GetReservation(runID string) (IdempotencyRecord, error) {
	var reservation IdempotencyRecord
	err := idx.db.QueryRow(`
SELECT capability, idempotency_key, run_id, request_id, request_fingerprint, request_json, run_json
FROM idempotency WHERE run_id = ? LIMIT 1
`, runID).Scan(&reservation.Capability, &reservation.Key, &reservation.RunID, &reservation.RequestID,
		&reservation.Fingerprint, &reservation.RequestJSON, &reservation.RunJSON)
	return reservation, err
}

func (idx *Index) List() ([]Record, error) {
	rows, err := idx.db.Query(`
SELECT id, request_id, status, source, capability, recipe, node_id, created_at, finished_at, state_dir, sync_status
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
			&item.Recipe, &item.NodeID, &item.CreatedAt, &item.FinishedAt, &item.StateDir, &item.SyncStatus); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if result == nil {
		result = []Record{}
	}
	return result, rows.Err()
}

// ListPage returns a stable keyset page ordered by createdAt DESC and id ASC.
// The opaque cursor is bound to its filters so it cannot accidentally resume
// a different query.
func (idx *Index) ListPage(query ListQuery) (Page, error) {
	limit := query.Limit
	if limit == 0 {
		limit = DefaultPageSize
	}
	if limit < 1 || limit > MaxPageSize {
		return Page{}, fmt.Errorf("%w: must be between 1 and %d", ErrInvalidLimit, MaxPageSize)
	}
	var cursor *listCursor
	if query.Cursor != "" {
		decoded, err := decodeListCursor(query.Cursor)
		if err != nil {
			return Page{}, err
		}
		if decoded.Capability != query.Capability || decoded.Status != query.Status {
			return Page{}, fmt.Errorf("%w: cursor filters do not match request", ErrInvalidCursor)
		}
		cursor = &decoded
	}

	var statement strings.Builder
	statement.WriteString(`
SELECT id, request_id, status, source, capability, recipe, node_id, created_at, finished_at, state_dir, sync_status
FROM runs WHERE 1 = 1`)
	args := make([]any, 0, 8)
	if query.Capability != "" {
		statement.WriteString(" AND capability = ?")
		args = append(args, query.Capability)
	}
	if query.Status != "" {
		statement.WriteString(" AND status = ?")
		args = append(args, query.Status)
	}
	if cursor != nil {
		statement.WriteString(" AND (created_at < ? OR (created_at = ? AND id > ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	statement.WriteString(" ORDER BY created_at DESC, id ASC LIMIT ?")
	args = append(args, limit+1)
	rows, err := idx.db.Query(statement.String(), args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	records := make([]Record, 0, limit+1)
	for rows.Next() {
		var item Record
		if err := rows.Scan(&item.ID, &item.RequestID, &item.Status, &item.Source, &item.Capability,
			&item.Recipe, &item.NodeID, &item.CreatedAt, &item.FinishedAt, &item.StateDir, &item.SyncStatus); err != nil {
			return Page{}, err
		}
		records = append(records, item)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	page := Page{Records: records}
	if len(records) > limit {
		page.Records = records[:limit]
		last := page.Records[len(page.Records)-1]
		page.NextCursor, err = encodeListCursor(listCursor{
			Version: 1, CreatedAt: last.CreatedAt, ID: last.ID,
			Capability: query.Capability, Status: query.Status,
		})
		if err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

// EncodeCursor builds an opaque keyset cursor bound to capability/status
// filters, resuming a ListPage strictly after the given (createdAt, id)
// boundary. It exists for callers that must resume beyond a single Index —
// e.g. cross-workspace aggregation, which fetches its own batch per
// workspace and needs to synthesize a resume point mid-batch.
func EncodeCursor(capability, status, createdAt, id string) (string, error) {
	return encodeListCursor(listCursor{Version: 1, CreatedAt: createdAt, ID: id, Capability: capability, Status: status})
}

func encodeListCursor(cursor listCursor) (string, error) {
	if _, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt); err != nil || !safeID.MatchString(cursor.ID) {
		return "", fmt.Errorf("%w: invalid indexed boundary", ErrInvalidCursor)
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeListCursor(value string) (listCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return listCursor{}, fmt.Errorf("%w: malformed encoding", ErrInvalidCursor)
	}
	var cursor listCursor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return listCursor{}, fmt.Errorf("%w: malformed payload", ErrInvalidCursor)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return listCursor{}, fmt.Errorf("%w: trailing payload", ErrInvalidCursor)
	}
	if cursor.Version != 1 || !safeID.MatchString(cursor.ID) {
		return listCursor{}, fmt.Errorf("%w: unsupported payload", ErrInvalidCursor)
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt); err != nil {
		return listCursor{}, fmt.Errorf("%w: invalid timestamp", ErrInvalidCursor)
	}
	return cursor, nil
}

func (idx *Index) Get(id string) (Record, error) {
	var item Record
	err := idx.db.QueryRow(`
SELECT id, request_id, status, source, capability, recipe, node_id, created_at, finished_at, state_dir, sync_status
FROM runs WHERE id = ?
`, id).Scan(&item.ID, &item.RequestID, &item.Status, &item.Source, &item.Capability,
		&item.Recipe, &item.NodeID, &item.CreatedAt, &item.FinishedAt, &item.StateDir, &item.SyncStatus)
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

// CommitTerminal atomically installs a terminal Run projection and its one
// durable sync item. It is safe to retry after any earlier partial write:
// existing outbox payload is repaired in place, while an already-acked Run
// (sync_status=synced) is never re-enqueued.
//
// Every read this function relies on happens inside the same transaction
// that performs the writes, after that transaction has forced acquisition of
// the database's single WAL write lock (see the leading no-op UPDATE below).
// Reading state outside the transaction (an earlier version of this
// function did, for lock-hold-time reasons) is a real TOCTOU race: a
// concurrent AckOutbox from a different *Index/connection can delete the
// outbox row and mark sync_status "synced" in the gap between this
// function's read and its write, and this function's write — computed from
// the stale pre-read snapshot — would then overwrite sync_status back to
// "pending" with no outbox row left to ever sync it. Forcing the lock before
// reading serializes the two operations instead: whichever one the SQLite
// engine schedules first fully completes (commits) before the other's read
// can observe anything, so the read is always of genuinely current state.
func (idx *Index) CommitTerminal(record Record, payload any) (bool, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	if record.Source == "" {
		record.Source = SourceLocal
	}

	tx, err := idx.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// database/sql.Begin issues a deferred BEGIN: no lock is actually held
	// until the first write statement. A no-op UPDATE forces that
	// acquisition immediately, before any read below, without depending on
	// whether record.ID already exists.
	if _, err := tx.Exec(`UPDATE runs SET id = id WHERE id = ?`, record.ID); err != nil {
		return false, err
	}
	if idx.testAfterCommitTerminalLock != nil {
		idx.testAfterCommitTerminalLock()
	}

	var previous Record
	err = tx.QueryRow(`
SELECT id, request_id, status, source, capability, recipe, node_id, created_at, finished_at, state_dir, sync_status
FROM runs WHERE id = ?
`, record.ID).Scan(&previous.ID, &previous.RequestID, &previous.Status, &previous.Source,
		&previous.Capability, &previous.Recipe, &previous.NodeID, &previous.CreatedAt,
		&previous.FinishedAt, &previous.StateDir, &previous.SyncStatus)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return false, err
	}

	changed := missing || previous.RequestID != record.RequestID || previous.Status != record.Status ||
		previous.Source != record.Source || previous.Capability != record.Capability || previous.Recipe != record.Recipe ||
		previous.NodeID != record.NodeID || previous.FinishedAt != record.FinishedAt || previous.StateDir != record.StateDir
	syncStatus := previous.SyncStatus
	if missing {
		syncStatus = record.SyncStatus
	}

	needsOutbox := false
	outboxPayload := string(data)
	if syncStatus != SyncSynced {
		var existingPayload string
		outboxErr := tx.QueryRow(`SELECT payload_json FROM outbox WHERE run_id = ?`, record.ID).Scan(&existingPayload)
		switch {
		case errors.Is(outboxErr, sql.ErrNoRows):
			needsOutbox = true
			changed = true
		case outboxErr != nil:
			return false, outboxErr
		case existingPayload != outboxPayload:
			needsOutbox = true
			changed = true
		}
		if syncStatus != SyncPending {
			changed = true
		}
		syncStatus = SyncPending
	}
	record.SyncStatus = syncStatus

	if needsOutbox {
		if _, err := tx.Exec(`
INSERT INTO outbox (run_id, payload_json, created_at, attempts)
VALUES (?, ?, ?, 0)
ON CONFLICT(run_id) DO UPDATE SET payload_json = excluded.payload_json
`, record.ID, outboxPayload, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return false, err
		}
	}
	_, err = tx.Exec(`
INSERT INTO runs (id, request_id, status, source, capability, recipe, node_id, created_at, finished_at, state_dir, sync_status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  request_id = excluded.request_id,
  status = excluded.status,
  source = excluded.source,
  capability = excluded.capability,
  recipe = excluded.recipe,
  node_id = excluded.node_id,
  finished_at = excluded.finished_at,
  state_dir = excluded.state_dir,
  sync_status = excluded.sync_status
`, record.ID, record.RequestID, record.Status, record.Source, record.Capability, record.Recipe,
		record.NodeID, record.CreatedAt, record.FinishedAt, record.StateDir, record.SyncStatus)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
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

// AckOutbox atomically removes the acked outbox item and marks the Run
// synced. Both statements are one transaction so a crash (or a concurrent
// CommitTerminal, serialized against this by the same forced-write-lock
// discipline — see CommitTerminal) can never observe or leave the outbox row
// deleted while sync_status still reads "pending".
func (idx *Index) AckOutbox(id int64, runID string) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM outbox WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE runs SET sync_status = ? WHERE id = ?`, SyncSynced, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (idx *Index) BumpOutboxAttempt(id int64) error {
	_, err := idx.db.Exec(`UPDATE outbox SET attempts = attempts + 1 WHERE id = ?`, id)
	return err
}
