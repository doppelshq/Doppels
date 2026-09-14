package runindex

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

type diskRun struct {
	ID         string    `json:"id"`
	RequestID  string    `json:"requestId"`
	CreatedAt  time.Time `json:"createdAt"`
	NodeID     string    `json:"nodeId"`
	Capability struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"capability"`
	Recipe *struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"recipe"`
}

type diskEvent struct {
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurredAt"`
}

// Backfill reconciles the index against .doppels/runs/<id> directories: it
// imports any Run missing from the index outright, and — since node_id and
// finished_at were added to the schema after some indexes were already
// populated (see migrate's additive ALTER TABLE), leaving existing rows
// defaulted to "" — enriches any already-indexed row still missing either
// field from the authoritative on-disk run.json/events.jsonl. A present,
// non-empty value is never overwritten: the index may already be more
// current than a disk snapshot (e.g. a Run still in flight).
func (idx *Index) Backfill() error {
	base := filepath.Join(idx.root, ".doppels", "runs")
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !safeID.MatchString(entry.Name()) {
			continue
		}
		existing, err := idx.Get(entry.Name())
		switch {
		case err == nil:
			if err := idx.enrichFromDisk(base, existing); err != nil {
				return fmt.Errorf("backfill %s: %w", entry.Name(), err)
			}
		case errors.Is(err, sql.ErrNoRows):
			record, err := recordFromDisk(base, entry.Name())
			if err != nil {
				return fmt.Errorf("backfill %s: %w", entry.Name(), err)
			}
			if err := idx.Upsert(record); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}

// enrichFromDisk fills a row's NodeID/FinishedAt from disk if either is
// still empty. It writes only those two columns via a single conditional
// UPDATE rather than a full-row Upsert of a Record read moments earlier: a
// live engine (runner.indexRun) can be Upserting the same row concurrently,
// and overwriting the whole row from a stale in-memory snapshot could
// clobber a status transition that landed in between the read and the
// write. The UPDATE's own WHEN clauses re-check emptiness at write time, so
// it can never step on a value the engine already set.
func (idx *Index) enrichFromDisk(base string, existing Record) error {
	if existing.NodeID != "" && existing.FinishedAt != "" {
		return nil
	}
	fresh, err := recordFromDisk(base, existing.ID)
	if errors.Is(err, os.ErrNotExist) {
		return nil // no run.json to enrich from; leave the indexed row as is.
	}
	if err != nil {
		return err
	}
	if fresh.NodeID == "" && fresh.FinishedAt == "" {
		return nil
	}
	if idx.testBeforeEnrichUpdate != nil {
		idx.testBeforeEnrichUpdate()
	}
	_, err = idx.db.Exec(`
UPDATE runs SET
  node_id = CASE WHEN node_id = '' THEN ? ELSE node_id END,
  finished_at = CASE WHEN finished_at = '' THEN ? ELSE finished_at END
WHERE id = ?
`, fresh.NodeID, fresh.FinishedAt, existing.ID)
	return err
}

func recordFromDisk(base, runID string) (Record, error) {
	dir := filepath.Join(base, runID)
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return Record{}, err
	}
	var run diskRun
	if err := json.Unmarshal(data, &run); err != nil {
		return Record{}, err
	}
	if run.ID == "" {
		run.ID = runID
	}
	status, finishedAt := statusAndFinishedAtFromEvents(filepath.Join(dir, "events.jsonl"))
	recipe := ""
	if run.Recipe != nil {
		recipe = run.Recipe.Name + "@" + run.Recipe.Version
	}
	created := run.CreatedAt.UTC().Format(time.RFC3339Nano)
	if run.CreatedAt.IsZero() {
		info, statErr := os.Stat(filepath.Join(dir, "run.json"))
		if statErr == nil {
			created = info.ModTime().UTC().Format(time.RFC3339Nano)
		}
	}
	return Record{
		ID: run.ID, RequestID: run.RequestID, Status: status, Source: SourceLocal,
		Capability: run.Capability.Name + "@" + run.Capability.Version, Recipe: recipe,
		NodeID: run.NodeID, CreatedAt: created, FinishedAt: finishedAt,
		StateDir: dir, SyncStatus: SyncNone,
	}, nil
}

// statusAndFinishedAtFromEvents derives status from the last terminal event
// in events.jsonl, and finishedAt from that same event's occurredAt — never
// from an unrelated (e.g. wall-clock) source, so it stays exact even when
// reconciling long after the Run actually finished.
func statusAndFinishedAtFromEvents(path string) (status, finishedAt string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "running", ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	status = "running"
	for _, line := range lines {
		if line == "" {
			continue
		}
		var event diskEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		switch event.Type {
		case "run_succeeded":
			status = "succeeded"
		case "run_failed":
			status = "failed"
		case "run_cancelled":
			status = "cancelled"
		case "run_interrupted":
			status = "interrupted"
		default:
			continue
		}
		finishedAt = ""
		if !event.OccurredAt.IsZero() {
			finishedAt = event.OccurredAt.UTC().Format(time.RFC3339Nano)
		}
	}
	return status, finishedAt
}
