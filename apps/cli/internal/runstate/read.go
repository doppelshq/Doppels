// Package runstate reads local Run state: the SQLite index for lists and the
// on-disk run directories for detail and logs. It never consults the Cloud.
package runstate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/runindex"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

type Summary struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Source     string `json:"source"`
	CreatedAt  string `json:"createdAt"`
	RequestID  string `json:"requestId"`
	Capability string `json:"capability"`
	Recipe     string `json:"recipe,omitempty"`
	StateDir   string `json:"stateDir,omitempty"`
	SyncStatus string `json:"syncStatus,omitempty"`
}

type Detail struct {
	Summary Summary                 `json:"summary"`
	Request execution.RequestRecord `json:"request"`
	Run     execution.RunRecord     `json:"run"`
	Events  []execution.RunEvent    `json:"events"`
}

type Log struct {
	Path    string `json:"path"`
	StepID  string `json:"stepId"`
	Stream  string `json:"stream"`
	Content string `json:"content"`
}

func List(root string) ([]Summary, error) {
	idx, err := runindex.Open(root)
	if err != nil {
		return nil, err
	}
	defer idx.Close()
	records, err := idx.List()
	if err != nil {
		return nil, err
	}
	result := make([]Summary, 0, len(records))
	for _, record := range records {
		result = append(result, Summary{
			ID: record.ID, Status: record.Status, Source: record.Source,
			CreatedAt: record.CreatedAt, RequestID: record.RequestID,
			Capability: record.Capability, Recipe: record.Recipe,
			StateDir: record.StateDir, SyncStatus: record.SyncStatus,
		})
	}
	return result, nil
}

func Load(root, runID string) (*Detail, error) {
	if !safeID.MatchString(runID) {
		return nil, fmt.Errorf("invalid Run id %q", runID)
	}
	dir := filepath.Join(root, ".doppels", "runs", runID)
	var request execution.RequestRecord
	if err := readJSON(filepath.Join(dir, "request.json"), &request); err != nil {
		return nil, err
	}
	var run execution.RunRecord
	if err := readJSON(filepath.Join(dir, "run.json"), &run); err != nil {
		return nil, err
	}
	if run.ID != runID || request.ID != run.RequestID {
		return nil, errors.New("local Run state has inconsistent identifiers")
	}
	events, err := readEvents(filepath.Join(dir, "events.jsonl"), runID)
	if err != nil {
		return nil, err
	}
	summary := Summary{
		ID: run.ID, Status: status(events), Source: runindex.SourceLocal,
		CreatedAt: run.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		RequestID: run.RequestID, Capability: reference(run.Capability), StateDir: dir,
	}
	if run.Recipe != nil {
		summary.Recipe = reference(*run.Recipe)
	}
	enrichStatusFromIndex(root, &summary)
	return &Detail{Summary: summary, Request: request, Run: run, Events: events}, nil
}

func enrichStatusFromIndex(root string, summary *Summary) {
	if summary == nil || summary.Status != "running" || summary.ID == "" {
		return
	}
	idx, err := runindex.Open(root)
	if err != nil {
		return
	}
	defer idx.Close()
	record, err := idx.Get(summary.ID)
	if err != nil || record.Status == "" {
		return
	}
	summary.Status = record.Status
}

func Logs(root, runID string) ([]Log, error) {
	detail, err := Load(root, runID)
	if err != nil {
		return nil, err
	}
	paths := logPaths(detail.Events)
	if len(paths) == 0 {
		entries, readErr := os.ReadDir(filepath.Join(detail.Summary.StateDir, "logs"))
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return nil, readErr
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				paths = append(paths, filepath.Join("logs", entry.Name()))
			}
		}
		sort.Strings(paths)
	}
	logs := make([]Log, 0, len(paths))
	for _, relative := range paths {
		clean := filepath.Clean(relative)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.Dir(clean) != "logs" {
			return nil, fmt.Errorf("RunEvent contains unsafe log path %q", relative)
		}
		data, err := os.ReadFile(filepath.Join(detail.Summary.StateDir, clean))
		if err != nil {
			return nil, err
		}
		name := filepath.Base(clean)
		stepID, stream := strings.TrimSuffix(name, ".log"), ""
		if before, after, ok := strings.Cut(stepID, "."); ok {
			stepID, stream = before, after
		}
		logs = append(logs, Log{Path: filepath.ToSlash(clean), StepID: stepID, Stream: stream, Content: string(data)})
	}
	return logs, nil
}

func readJSON(path string, output any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func readEvents(path, runID string) ([]execution.RunEvent, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []execution.RunEvent{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var events []execution.RunEvent
	scanner := bufio.NewScanner(io.LimitReader(file, 16<<20))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var event execution.RunEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		if event.RunID != runID || event.Sequence != len(events) {
			return nil, errors.New("local RunEvent sequence is inconsistent")
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func status(events []execution.RunEvent) string {
	for index := len(events) - 1; index >= 0; index-- {
		switch events[index].Type {
		case "run_succeeded":
			return "succeeded"
		case "run_failed":
			return "failed"
		case "run_cancelled":
			return "cancelled"
		case "run_interrupted":
			return "interrupted"
		}
	}
	return "running"
}

func reference(value execution.DefinitionReference) string {
	return value.Name + "@" + value.Version
}

func logPaths(events []execution.RunEvent) []string {
	seen := map[string]struct{}{}
	var paths []string
	for _, event := range events {
		for _, name := range []string{"stdout", "stderr"} {
			value, ok := event.Data[name].(string)
			if !ok || value == "" {
				continue
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			paths = append(paths, value)
		}
	}
	return paths
}
