package command

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/shareclient"
)

func TestListenStatusPausedDuringDecision(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	var mu sync.Mutex
	pause := &atomic.Bool{}
	pause.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			if !pause.Load() {
				mu.Lock()
				writeListenStatus(&out, 2, 1, 1)
				mu.Unlock()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	interaction := newInteraction(strings.NewReader("a\n"), &out)
	now := time.Now().UTC()
	request := execution.RequestRecord{
		ID:          "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		RequestedBy: execution.ActorReference{Kind: "guest", ID: "guest"},
		Capability:  execution.DefinitionReference{Name: "greet", Version: "1.0.0"},
		Inputs:      map[string]any{"audience": "Ada"},
	}
	created := &shareclient.ShareCreated{
		Share: shareclient.Share{
			ID:        "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			ExpiresAt: now.Add(time.Hour),
		},
	}

	mu.Lock()
	writeListenDecisionPrompt(&out, request, created, listenPromptQueue{Index: 1, Total: 1}, now)
	mu.Unlock()

	got, err := interaction.decideFulfillment()
	if err != nil || got != fulfillApprove {
		t.Fatalf("decide: got=%v err=%v", got, err)
	}
	pause.Store(false)
	<-done

	text := out.String()
	if !strings.Contains(text, "[a] Approve") {
		t.Fatalf("missing prompt in:\n%s", text)
	}
	cardAt := strings.Index(text, "════════════════════════════════════════")
	promptAt := strings.Index(text, "[a] Approve")
	if cardAt < 0 || promptAt < 0 {
		t.Fatalf("card/prompt missing:\n%s", text)
	}
	between := text[cardAt:promptAt]
	if strings.Contains(between, "Shares") || strings.Contains(between, "to decide") {
		t.Fatalf("status clobbered prompt region:\n%s", between)
	}
}
