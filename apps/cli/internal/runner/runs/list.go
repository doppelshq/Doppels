package runs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"doppels.so/cli/internal/runindex"
	"doppels.so/cli/internal/runner/proto"
)

// ListParams is the v1/listRuns request. An empty Workspace aggregates every
// registered workspace.
type ListParams struct {
	Workspace  string
	Capability string
	Status     string
	Limit      int
	Cursor     string
}

// ListResult is the v1/listRuns response, ordered createdAt DESC.
type ListResult struct {
	Runs       []proto.RunSummary `json:"runs"`
	NextCursor string             `json:"nextCursor,omitempty"`
}

// ListRuns lists a single workspace's Runs via the durable keyset page, or
// aggregates every registered workspace by merging each one's page in
// createdAt DESC order.
func (m *Manager) ListRuns(params ListParams) (ListResult, *proto.Error) {
	if params.Limit == 0 {
		params.Limit = runindex.DefaultPageSize
	}
	if params.Limit < 1 || params.Limit > runindex.MaxPageSize {
		return ListResult{}, invalidParams(fmt.Sprintf("limit must be between 1 and %d", runindex.MaxPageSize))
	}
	if params.Workspace != "" {
		return m.listRunsForRoot(params.Workspace, params)
	}
	return m.listRunsAcrossWorkspaces(params)
}

func (m *Manager) listRunsForRoot(root string, params ListParams) (ListResult, *proto.Error) {
	root, protoErr := m.resolveRoot(root)
	if protoErr != nil {
		return ListResult{}, protoErr
	}
	idx, err := m.index(root)
	if err != nil {
		return ListResult{}, internalError(err)
	}
	page, err := idx.ListPage(runindex.ListQuery{Capability: params.Capability, Status: params.Status, Limit: params.Limit, Cursor: params.Cursor})
	if err != nil {
		return ListResult{}, listPageError(err)
	}
	runs := make([]proto.RunSummary, 0, len(page.Records))
	for _, record := range page.Records {
		runs = append(runs, summaryFromRecord(root, record))
	}
	return ListResult{Runs: runs, NextCursor: page.NextCursor}, nil
}

func (m *Manager) resolveRoot(workspace string) (string, *proto.Error) {
	for _, summary := range m.workspaces.ListWorkspaces() {
		if summary.Root != workspace {
			continue
		}
		if summary.Health == "missingRoot" {
			return "", &proto.Error{Code: proto.CodeWorkspaceNotFound, Message: "workspace is registered but missing"}
		}
		return summary.Root, nil
	}
	return "", &proto.Error{Code: proto.CodeWorkspaceNotFound, Message: "workspace not found: " + workspace}
}

// crossCursor resumes cross-workspace aggregation. Roots maps each
// still-active workspace root to the cursor it should resume from (an empty
// string means "from the start" for a root introduced since the last page).
type crossCursor struct {
	Version    int               `json:"v"`
	Capability string            `json:"capability"`
	Status     string            `json:"status"`
	Roots      map[string]string `json:"roots"`
}

type rootBatch struct {
	root       string
	records    []runindex.Record
	nextCursor string
}

func (m *Manager) listRunsAcrossWorkspaces(params ListParams) (ListResult, *proto.Error) {
	roots := map[string]string{}
	if params.Cursor != "" {
		decoded, err := decodeCrossCursor(params.Cursor)
		if err != nil {
			return ListResult{}, invalidParams(err.Error())
		}
		if decoded.Capability != params.Capability || decoded.Status != params.Status {
			return ListResult{}, invalidParams("cursor filters do not match request")
		}
		// The cursor is client-supplied and opaque: its "roots" are never
		// authoritative on their own. m.index() creates a .doppels
		// directory as a side effect (runindex.Open), so a forged cursor
		// or one naming a workspace removed since it was issued must be
		// rejected here — before any lookup — never used to touch the
		// filesystem outside the current registry.
		registered := map[string]bool{}
		for _, summary := range m.workspaces.ListWorkspaces() {
			if summary.Health != "missingRoot" {
				registered[summary.Root] = true
			}
		}
		for root := range decoded.Roots {
			if !registered[root] {
				return ListResult{}, &proto.Error{Code: proto.CodeWorkspaceNotFound, Message: "cursor references a workspace that is no longer registered: " + root}
			}
		}
		roots = decoded.Roots
	} else {
		for _, summary := range m.workspaces.ListWorkspaces() {
			if summary.Health == "missingRoot" {
				continue
			}
			roots[summary.Root] = ""
		}
	}

	batches := make([]rootBatch, 0, len(roots))
	for root, cursor := range roots {
		idx, err := m.index(root)
		if err != nil {
			return ListResult{}, internalError(err)
		}
		page, err := idx.ListPage(runindex.ListQuery{Capability: params.Capability, Status: params.Status, Limit: params.Limit, Cursor: cursor})
		if err != nil {
			return ListResult{}, listPageError(err)
		}
		batches = append(batches, rootBatch{root: root, records: page.Records, nextCursor: page.NextCursor})
	}

	type item struct {
		root   string
		record runindex.Record
	}
	merged := make([]item, 0)
	for _, batch := range batches {
		for _, record := range batch.records {
			merged = append(merged, item{root: batch.root, record: record})
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].record.CreatedAt != merged[j].record.CreatedAt {
			return merged[i].record.CreatedAt > merged[j].record.CreatedAt
		}
		return merged[i].record.ID < merged[j].record.ID
	})
	if len(merged) > params.Limit {
		merged = merged[:params.Limit]
	}

	consumed := map[string]int{}
	runs := make([]proto.RunSummary, 0, len(merged))
	for _, entry := range merged {
		runs = append(runs, summaryFromRecord(entry.root, entry.record))
		consumed[entry.root]++
	}

	nextRoots := map[string]string{}
	for _, batch := range batches {
		n := consumed[batch.root]
		switch {
		case n == 0:
			// Nothing from this root made the cut; it resumes at the same
			// position (its fetched batch is simply re-fetched next time).
			nextRoots[batch.root] = roots[batch.root]
		case n < len(batch.records):
			// Partially consumed: resume right after the last item we
			// returned, not at the root's own page boundary (which would
			// skip the unconsumed remainder of this batch).
			boundary := batch.records[n-1]
			cursor, err := runindex.EncodeCursor(params.Capability, params.Status, boundary.CreatedAt, boundary.ID)
			if err != nil {
				return ListResult{}, internalError(err)
			}
			nextRoots[batch.root] = cursor
		case batch.nextCursor != "":
			// Fully consumed this batch, but the root itself has more.
			nextRoots[batch.root] = batch.nextCursor
		}
	}

	result := ListResult{Runs: runs}
	if len(nextRoots) > 0 {
		cursor, err := encodeCrossCursor(crossCursor{Version: 1, Capability: params.Capability, Status: params.Status, Roots: nextRoots})
		if err != nil {
			return ListResult{}, internalError(err)
		}
		result.NextCursor = cursor
	}
	return result, nil
}

func encodeCrossCursor(cursor crossCursor) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeCrossCursor(value string) (crossCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return crossCursor{}, fmt.Errorf("%w: malformed encoding", runindex.ErrInvalidCursor)
	}
	var cursor crossCursor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return crossCursor{}, fmt.Errorf("%w: malformed payload", runindex.ErrInvalidCursor)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return crossCursor{}, fmt.Errorf("%w: trailing payload", runindex.ErrInvalidCursor)
	}
	if cursor.Version != 1 || len(cursor.Roots) == 0 {
		return crossCursor{}, fmt.Errorf("%w: unsupported payload", runindex.ErrInvalidCursor)
	}
	return cursor, nil
}

func listPageError(err error) *proto.Error {
	if errors.Is(err, runindex.ErrInvalidCursor) || errors.Is(err, runindex.ErrInvalidLimit) {
		return invalidParams(err.Error())
	}
	return internalError(err)
}
