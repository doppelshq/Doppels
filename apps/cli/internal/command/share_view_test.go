package command

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/shareclient"
)

func TestWriteShareSessionPinsRecipeRevision(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeShareSessionHuman(&buf, &shareclient.ShareCreated{
		PublicURL: "http://localhost:4000/s/escalate-ticket-mvhhchhh44",
		Share: shareclient.Share{
			ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
			CapabilityRevision: execution.DefinitionReference{
				Name: "escalate-ticket", Version: "1.1.0",
			},
			Recipe: &execution.DefinitionReference{
				Name: "escalate-ticket-zendesk", Version: "1.1.0",
			},
		},
	}, time.Now().UTC(), true)
	out := buf.String()
	if !strings.Contains(out, "escalate-ticket-zendesk@1.1.0") {
		t.Fatalf("Recipe must show pinned @version:\n%s", out)
	}
	if strings.Contains(out, "escalate-ticket@1.1.0") {
		t.Fatalf("Cap should stay unversioned:\n%s", out)
	}
}

func TestWriteSharedRequestOmitsFiller(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeSharedRequest(&buf, execution.RequestRecord{
		RequestedBy: execution.ActorReference{ID: "guest"},
		Inputs:      map[string]any{"ticket-id": "dsdssd"},
	})
	out := buf.String()
	if strings.Contains(out, "Fulfilling locally") {
		t.Fatalf("filler still present:\n%s", out)
	}
	if !strings.Contains(out, "ticket-id") || !strings.Contains(out, "dsdssd") {
		t.Fatalf("missing input:\n%s", out)
	}
}
