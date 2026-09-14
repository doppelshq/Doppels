package runs

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runstate"
)

// maxLogPageBytes bounds a single getRunLogs content page (RFC §4: 4 MiB
// frame limit; log payloads must fit inside one wire message).
const maxLogPageBytes = 4 << 20

// LogsParams is the v1/getRunLogs request.
type LogsParams struct {
	RunID  string
	StepID string
	Offset int
	Limit  int
}

// LogFile describes one confined per-(step,stream) log file.
type LogFile struct {
	StepID    string `json:"stepId"`
	Stream    string `json:"stream"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

// LogsResult is the v1/getRunLogs response. Content is only populated when
// StepID narrows the request: it is the byte-paginated concatenation of that
// step's matching streams (stdout, then stderr), so a single stepId with two
// streams still yields one deterministic, offset-addressable body.
type LogsResult struct {
	Files   []LogFile `json:"files"`
	Content *string   `json:"content,omitempty"`
}

var truncationMarker = regexp.MustCompile(`\n\[doppels: truncated after [^\]]+\]\n$`)

// GetRunLogs lists the confined log files for a Run (optionally filtered to
// one Step) and, when a Step is given, returns a paginated content window.
func (m *Manager) GetRunLogs(params LogsParams) (LogsResult, *proto.Error) {
	if !safeRunID(params.RunID) {
		return LogsResult{}, &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	root, _, _, err := m.findRecord(params.RunID)
	if errors.Is(err, sql.ErrNoRows) {
		return LogsResult{}, &proto.Error{Code: proto.CodeRunNotFound, Message: "Run not found"}
	}
	if err != nil {
		return LogsResult{}, internalError(err)
	}
	logs, err := runstate.Logs(root, params.RunID)
	if err != nil {
		return LogsResult{}, internalError(err)
	}

	files := make([]LogFile, 0, len(logs))
	matched := make([]runstate.Log, 0, len(logs))
	stateDir := filepath.Join(root, ".doppels", "runs", params.RunID)
	for _, entry := range logs {
		if params.StepID != "" && entry.StepID != params.StepID {
			continue
		}
		var size int64
		if info, statErr := os.Stat(filepath.Join(stateDir, filepath.FromSlash(entry.Path))); statErr == nil {
			size = info.Size()
		}
		files = append(files, LogFile{
			StepID: entry.StepID, Stream: entry.Stream, Path: entry.Path,
			Size: size, Truncated: truncationMarker.MatchString(entry.Content),
		})
		matched = append(matched, entry)
	}
	result := LogsResult{Files: files}
	if params.StepID == "" {
		return result, nil
	}

	sort.SliceStable(matched, func(i, j int) bool { return streamOrder(matched[i].Stream) < streamOrder(matched[j].Stream) })
	var combined strings.Builder
	for _, entry := range matched {
		combined.WriteString(entry.Content)
	}
	full := combined.String()
	offset := params.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > len(full) {
		offset = len(full)
	}
	limit := params.Limit
	if limit <= 0 || limit > maxLogPageBytes {
		limit = maxLogPageBytes
	}
	end := offset + limit
	if end > len(full) {
		end = len(full)
	}
	page := full[offset:end]
	result.Content = &page
	return result, nil
}

func streamOrder(stream string) int {
	switch stream {
	case "stdout":
		return 0
	case "stderr":
		return 1
	default:
		return 2
	}
}
