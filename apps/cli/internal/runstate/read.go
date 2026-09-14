// Package runstate reads local Run state: the SQLite index for lists and the
// on-disk run directories for detail and logs. It never consults the Cloud.
package runstate

import (
	"bufio"
	"bytes"
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
	return load(root, runID, nil)
}

type RecordGetter interface {
	Get(id string) (runindex.Record, error)
}

// LoadWithIndex reuses a coordinator-owned long-lived index instead of
// opening a short-lived SQLite handle while enriching the detail status.
func LoadWithIndex(root, runID string, idx RecordGetter) (*Detail, error) {
	return load(root, runID, idx)
}

func load(root, runID string, idx RecordGetter) (*Detail, error) {
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
	enrichStatusFromIndex(root, idx, &summary)
	return &Detail{Summary: summary, Request: request, Run: run, Events: events}, nil
}

func enrichStatusFromIndex(root string, idx RecordGetter, summary *Summary) {
	if summary == nil || summary.Status != "running" || summary.ID == "" {
		return
	}
	if idx == nil {
		opened, err := runindex.Open(root)
		if err != nil {
			return
		}
		defer opened.Close()
		idx = opened
	}
	record, err := idx.Get(summary.ID)
	if err != nil || record.Status == "" {
		return
	}
	summary.Status = record.Status
}

// LogFileRef is a confined, symlink-resolved reference to one (step, stream)
// log file discovered from a Run's events (or, absent those, its logs/
// directory). RealPath has already been proven to resolve inside the Run's
// state directory: callers may open it directly without repeating that
// check. Discovery never reads file content, so it stays cheap even when a
// stream is at its 16 MiB engine cap.
type LogFileRef struct {
	Path      string // relative, slash-separated (e.g. "logs/run.stdout.log")
	StepID    string
	Stream    string
	RealPath  string
	Size      int64
	Truncated bool
}

// LogFiles discovers and confines a Run's log files without reading their
// content, for callers that need to paginate a file's bytes (getRunLogs)
// instead of loading it whole.
func LogFiles(root, runID string) ([]LogFileRef, error) {
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
	realStateDir, err := filepath.EvalSymlinks(detail.Summary.StateDir)
	if err != nil {
		return nil, err
	}
	refs := make([]LogFileRef, 0, len(paths))
	for _, relative := range paths {
		clean := filepath.Clean(relative)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.Dir(clean) != "logs" {
			return nil, fmt.Errorf("RunEvent contains unsafe log path %q", relative)
		}
		// The name alone can never prove safety: a symlink at this path
		// (planted by a Recipe step, which runs arbitrary shell) can point
		// anywhere on disk while looking like an ordinary log file. Resolve
		// the real path and confine it to the Run directory before reading,
		// mirroring the pattern localstate.Store.Resume already uses.
		resolved, err := filepath.EvalSymlinks(filepath.Join(detail.Summary.StateDir, clean))
		if err != nil {
			return nil, err
		}
		relToRoot, err := filepath.Rel(realStateDir, resolved)
		if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("log path %q escapes the Run directory", relative)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, err
		}
		truncated, err := hasTruncationMarker(resolved, info.Size())
		if err != nil {
			return nil, err
		}
		name := filepath.Base(clean)
		stepID, stream := strings.TrimSuffix(name, ".log"), ""
		if before, after, ok := strings.Cut(stepID, "."); ok {
			stepID, stream = before, after
		}
		refs = append(refs, LogFileRef{
			Path: filepath.ToSlash(clean), StepID: stepID, Stream: stream,
			RealPath: resolved, Size: info.Size(), Truncated: truncated,
		})
	}
	return refs, nil
}

// truncationMarkerNeedle is the fixed prefix of the marker the engine
// appends when a stream hit its cap (execution.finalizeLogBytes); the
// suffix varies with the configured limit ("16MiB", "512KiB", ...), so only
// the stable prefix is checked.
const truncationMarkerNeedle = "[doppels: truncated after "

// truncationProbeBytes bounds how much of a file's tail hasTruncationMarker
// reads: comfortably larger than the marker itself, tiny next to a 16 MiB
// stream cap.
const truncationProbeBytes = 256

func hasTruncationMarker(path string, size int64) (bool, error) {
	start := size - truncationProbeBytes
	if start < 0 {
		start = 0
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	buf := make([]byte, size-start)
	if _, err := file.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return bytes.Contains(buf, []byte(truncationMarkerNeedle)), nil
}

// Logs reads every discovered log file's full content. Prefer LogFiles for
// callers that only need metadata or a byte range: this loads each file
// entirely into memory, which is fine for CLI display but wasteful (and, at
// the engine's 16 MiB per-stream cap, wire-unsafe) for paginated RPC access.
func Logs(root, runID string) ([]Log, error) {
	refs, err := LogFiles(root, runID)
	if err != nil {
		return nil, err
	}
	logs := make([]Log, 0, len(refs))
	for _, ref := range refs {
		data, err := os.ReadFile(ref.RealPath)
		if err != nil {
			return nil, err
		}
		logs = append(logs, Log{Path: ref.Path, StepID: ref.StepID, Stream: ref.Stream, Content: string(data)})
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
