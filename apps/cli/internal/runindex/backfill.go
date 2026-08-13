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
	Type string `json:"type"`
}

// Backfill imports .doppels/runs/<id> directories that are missing from the index.
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
		_, err := idx.Get(entry.Name())
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		record, err := recordFromDisk(base, entry.Name())
		if err != nil {
			return fmt.Errorf("backfill %s: %w", entry.Name(), err)
		}
		if err := idx.Upsert(record); err != nil {
			return err
		}
	}
	return nil
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
	status := statusFromEvents(filepath.Join(dir, "events.jsonl"))
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
		CreatedAt: created, StateDir: dir, SyncStatus: SyncNone,
	}, nil
}

func statusFromEvents(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "running"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	status := "running"
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
		}
	}
	return status
}
