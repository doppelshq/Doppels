package runs

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"sort"

	"doppels.so/cli/internal/runner/proto"
	"doppels.so/cli/internal/runstate"
)

// logFrameOverheadBytes reserves room, on top of the base64-encoded content
// itself, for the JSON-RPC envelope and files[] metadata (one small,
// bounded entry per discovered log file).
const logFrameOverheadBytes = 64 << 10

// maxLogContentBytes bounds the *raw* (pre-base64) bytes a single
// getRunLogs content page may span. Base64 expands raw bytes by exactly
// 4/3 (rounded up); capping the raw window here keeps the fully encoded
// response safely under proto.MaxFrameBytes, which the transport enforces
// by refusing to write an oversized frame — never something a paginated,
// read-only RPC should be able to trigger.
const maxLogContentBytes = (proto.MaxFrameBytes - logFrameOverheadBytes) / 4 * 3

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

// LogsResult is the v1/getRunLogs response. Content, when present, is the
// base64 encoding of the raw byte range [offset, offset+limit) of the
// matched step's streams concatenated in stdout-then-stderr order. Log
// output is arbitrary subprocess bytes, not guaranteed valid UTF-8: a plain
// JSON string would let json.Marshal silently replace invalid sequences
// with U+FFFD, and a byte-offset page can legitimately split a multi-byte
// rune in half. Base64 keeps the offset/limit contract byte-exact
// regardless of what the subprocess wrote.
type LogsResult struct {
	Files   []LogFile `json:"files"`
	Content *string   `json:"content,omitempty"`
}

// GetRunLogs lists the confined log files for a Run (optionally filtered to
// one Step) and, when a Step is given, returns a paginated content window.
// It never loads a full log file into memory: LogFiles discovers and
// confines paths only, and readWindow opens just the file(s) a requested
// byte range actually intersects.
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
	refs, err := runstate.LogFiles(root, params.RunID)
	if err != nil {
		return LogsResult{}, internalError(err)
	}

	files := make([]LogFile, 0, len(refs))
	matched := make([]runstate.LogFileRef, 0, len(refs))
	for _, ref := range refs {
		if params.StepID != "" && ref.StepID != params.StepID {
			continue
		}
		files = append(files, LogFile{StepID: ref.StepID, Stream: ref.Stream, Path: ref.Path, Size: ref.Size, Truncated: ref.Truncated})
		matched = append(matched, ref)
	}
	result := LogsResult{Files: files}
	if params.StepID == "" {
		return result, nil
	}

	sort.SliceStable(matched, func(i, j int) bool { return streamOrder(matched[i].Stream) < streamOrder(matched[j].Stream) })
	var total int64
	for _, ref := range matched {
		total += ref.Size
	}
	offset := int64(params.Offset)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	limit := int64(params.Limit)
	if limit <= 0 || limit > maxLogContentBytes {
		limit = maxLogContentBytes
	}
	end := offset + limit
	if end > total {
		end = total
	}

	window, err := readWindow(matched, offset, end)
	if err != nil {
		return LogsResult{}, internalError(err)
	}
	encoded := base64.StdEncoding.EncodeToString(window)
	result.Content = &encoded
	return result, nil
}

// readWindow reads exactly the raw bytes in [start, end) from the logical
// concatenation of refs, opening and seeking into only the file(s) that
// range actually intersects.
func readWindow(refs []runstate.LogFileRef, start, end int64) ([]byte, error) {
	if end <= start {
		return []byte{}, nil
	}
	result := make([]byte, 0, end-start)
	var consumed int64
	for _, ref := range refs {
		fileStart := consumed
		fileEnd := consumed + ref.Size
		consumed = fileEnd
		if fileEnd <= start || fileStart >= end {
			continue
		}
		chunk, err := readFileRange(ref.RealPath, max64(start, fileStart)-fileStart, min64(end, fileEnd)-fileStart)
		if err != nil {
			return nil, err
		}
		result = append(result, chunk...)
	}
	return result, nil
}

func readFileRange(path string, start, end int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buf := make([]byte, end-start)
	if _, err := file.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
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
