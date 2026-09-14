package runs

import (
	"context"
	"os"
	"testing"

	"doppels.so/cli/internal/runner/proto"
)

func TestGetRunLogsListsFilesAndPaginatesContentForAStep(t *testing.T) {
	service, root := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test", Environment: []string{"PATH=" + os.Getenv("PATH")}})
	defer manager.Close()

	started := startAndFinish(t, manager, root, "logs-1")

	result, rpcErr := manager.GetRunLogs(LogsParams{RunID: started.RunID})
	if rpcErr != nil {
		t.Fatalf("GetRunLogs: %+v", rpcErr)
	}
	if len(result.Files) == 0 {
		t.Fatal("expected at least one log file")
	}
	if result.Content != nil {
		t.Fatal("content must be omitted without a stepId")
	}
	stepID := result.Files[0].StepID

	withStep, rpcErr := manager.GetRunLogs(LogsParams{RunID: started.RunID, StepID: stepID})
	if rpcErr != nil {
		t.Fatalf("GetRunLogs with stepId: %+v", rpcErr)
	}
	if withStep.Content == nil {
		t.Fatal("content must be present with a stepId")
	}
	for _, file := range withStep.Files {
		if file.StepID != stepID {
			t.Fatalf("files leaked another step: %#v", withStep.Files)
		}
	}
	full := *withStep.Content

	paged, rpcErr := manager.GetRunLogs(LogsParams{RunID: started.RunID, StepID: stepID, Offset: 1, Limit: 2})
	if rpcErr != nil {
		t.Fatalf("GetRunLogs paginated: %+v", rpcErr)
	}
	if paged.Content == nil {
		t.Fatal("paginated content missing")
	}
	if len(full) >= 3 && *paged.Content != full[1:3] {
		t.Fatalf("paginated content = %q, want %q", *paged.Content, full[1:3])
	}
}

func TestGetRunLogsUnknownRunReturnsRunNotFound(t *testing.T) {
	service, _ := runnerWorkspace(t, true)
	manager := NewManager(context.Background(), service, Config{NodeID: "node-test"})
	defer manager.Close()

	if _, rpcErr := manager.GetRunLogs(LogsParams{RunID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}); rpcErr == nil || rpcErr.Code != proto.CodeRunNotFound {
		t.Fatalf("GetRunLogs unknown = %+v, want -32006", rpcErr)
	}
}
