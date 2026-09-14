package listener

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/shareclient"
)

// fakeRegistry serves /api/v1/listen/inbox from a canned payload under a
// mutex so tests can mutate it between polls.
type fakeRegistry struct {
	mu       sync.Mutex
	payload  map[string]any
	polls    int
	failWith int
}

func (f *fakeRegistry) inboxPayload(payload map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payload = payload
}

func (f *fakeRegistry) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = status
}

func (f *fakeRegistry) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.failWith != 0 {
		w.WriteHeader(f.failWith)
		_, _ = w.Write([]byte(`{"error":"broken"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.payload)
}

func spaceRequestPayload(requestID, status string) map[string]any {
	return map[string]any{
		"scopes": []map[string]any{
			{"organization": "acme", "space": "infra", "capabilities": []map[string]any{}},
		},
		"shares": []map[string]any{},
		"requests": []map[string]any{{
			"apiVersion":     "doppels.dev/v1alpha1",
			"kind":           "Request",
			"id":             requestID,
			"idempotencyKey": "local:" + requestID,
			"organization":   "acme",
			"space":          "infra",
			"status":         status,
			"capability": map[string]any{
				"name":    "drain-node",
				"version": "1.0.0",
			},
			"inputs": map[string]any{"host": "edge-1"},
		}},
	}
}

func newListenerClients(t *testing.T, registry *fakeRegistry) (*registryclient.Client, *shareclient.Client) {
	t.Helper()
	server := httptest.NewServer(registry)
	t.Cleanup(server.Close)
	reg, err := registryclient.New(server.URL, server.Client())
	if err != nil {
		t.Fatalf("registry client: %v", err)
	}
	shares, err := shareclient.New(shareclient.Options{Server: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return time.Now().UTC() }})
	if err != nil {
		t.Fatalf("share client: %v", err)
	}
	return reg, shares
}

func waitForJobs(t *testing.T, jobs <-chan Job, want int) []Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got []Job
	for time.Now().Before(deadline) {
		for len(got) < want {
			select {
			case job := <-jobs:
				got = append(got, job)
				continue
			default:
			}
			break
		}
		if len(got) >= want {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d jobs, got %d", want, len(got))
	return nil
}

func TestProducerEmitsSpaceRequestJobs(t *testing.T) {
	registry := &fakeRegistry{payload: spaceRequestPayload("req-1", "requested")}
	reg, shares := newListenerClients(t, registry)
	jobs := make(chan Job, 16)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunProducer(ctx, ProducerConfig{
			Registry:  reg,
			Shares:    shares,
			Token:     "token-1",
			Filters:   Filters{Organization: "acme"},
			PollEvery: 10 * time.Millisecond,
			Jobs:      jobs,
		})
	}()
	t.Cleanup(cancel)

	got := waitForJobs(t, jobs, 1)
	job := got[0]
	if job.Origin != "space" {
		t.Fatalf("origin = %q, want space", job.Origin)
	}
	if job.Organization != "acme" || job.Space != "infra" {
		t.Fatalf("scope = %s/%s, want acme/infra", job.Organization, job.Space)
	}
	if job.Request == nil || job.Request.ID != "req-1" {
		t.Fatalf("request = %+v, want req-1", job.Request)
	}
	if job.Request.Capability.Name != "drain-node" {
		t.Fatalf("capability = %s, want drain-node", job.Request.Capability.Name)
	}
}

func TestProducerDeduplicatesAcrossPolls(t *testing.T) {
	registry := &fakeRegistry{payload: spaceRequestPayload("req-1", "requested")}
	reg, shares := newListenerClients(t, registry)
	jobs := make(chan Job, 16)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunProducer(ctx, ProducerConfig{
			Registry:  reg,
			Shares:    shares,
			Token:     "token-1",
			Filters:   Filters{Organization: "acme"},
			PollEvery: 5 * time.Millisecond,
			Jobs:      jobs,
		})
	}()
	t.Cleanup(cancel)

	waitForJobs(t, jobs, 1)

	// Give the producer several extra polls with the same payload.
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case job := <-jobs:
			t.Fatalf("duplicate job emitted: %+v", job)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	if registry.pollCount() < 2 {
		t.Fatalf("expected multiple polls, got %d", registry.pollCount())
	}
}

func TestProducerSkipsUnactionableRequests(t *testing.T) {
	registry := &fakeRegistry{payload: map[string]any{
		"scopes":   []map[string]any{},
		"shares":   []map[string]any{},
		"requests": []map[string]any{},
	}}
	reg, shares := newListenerClients(t, registry)
	// Completed request: not actionable.
	registry.inboxPayload(spaceRequestPayload("req-done", "completed"))

	jobs := make(chan Job, 16)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunProducer(ctx, ProducerConfig{
			Registry:  reg,
			Shares:    shares,
			Token:     "token-1",
			Filters:   Filters{Organization: "acme"},
			PollEvery: 5 * time.Millisecond,
			Jobs:      jobs,
		})
	}()
	t.Cleanup(cancel)

	deadline := time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case job := <-jobs:
			t.Fatalf("unactionable request emitted job: %+v", job)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Missing organization: cannot route.
	missingScope := spaceRequestPayload("req-noscope", "requested")
	delete(missingScope["requests"].([]map[string]any)[0], "organization")
	registry.inboxPayload(missingScope)
	deadline = time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case job := <-jobs:
			t.Fatalf("scopeless request emitted job: %+v", job)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProducerPropagatesPollError(t *testing.T) {
	registry := &fakeRegistry{payload: spaceRequestPayload("req-1", "requested")}
	reg, shares := newListenerClients(t, registry)
	registry.fail(http.StatusInternalServerError)

	jobs := make(chan Job, 16)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunProducer(ctx, ProducerConfig{
			Registry:  reg,
			Shares:    shares,
			Token:     "token-1",
			Filters:   Filters{Organization: "acme"},
			PollEvery: 10 * time.Millisecond,
			Jobs:      jobs,
		})
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from failed poll")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for poll error")
	}
	// Jobs channel must close after the producer exits.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, open := <-jobs; !open {
			return
		}
		t.Fatal("jobs channel emitted data after producer error")
	}
	t.Fatal("jobs channel never closed after producer error")
}

func TestProducerClosesJobsOnContextCancel(t *testing.T) {
	registry := &fakeRegistry{payload: map[string]any{
		"scopes":   []map[string]any{},
		"shares":   []map[string]any{},
		"requests": []map[string]any{},
	}}
	reg, shares := newListenerClients(t, registry)
	jobs := make(chan Job, 16)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunProducer(ctx, ProducerConfig{
			Registry:  reg,
			Shares:    shares,
			Token:     "token-1",
			Filters:   Filters{Organization: "acme"},
			PollEvery: 5 * time.Millisecond,
			Jobs:      jobs,
		})
	}()

	waitForPolls(t, registry, 2)
	cancel()

	select {
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not exit on cancel")
	}
	if _, open := <-jobs; open {
		t.Fatal("jobs channel still open after producer exit")
	}
}

func TestProducerReportsScopeStatus(t *testing.T) {
	registry := &fakeRegistry{payload: spaceRequestPayload("req-1", "requested")}
	reg, shares := newListenerClients(t, registry)

	reporter := &recordingReporter{}
	jobs := make(chan Job, 16)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunProducer(ctx, ProducerConfig{
			Registry:  reg,
			Shares:    shares,
			Token:     "token-1",
			Filters:   Filters{Organization: "acme"},
			PollEvery: 5 * time.Millisecond,
			Jobs:      jobs,
			Reporter:  reporter,
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counts := reporter.lastCounts(); counts != nil {
			if counts.Scopes != 1 {
				t.Fatalf("scopes = %d, want 1", counts.Scopes)
			}
			if counts.Requests != 1 {
				t.Fatalf("requests = %d, want 1", counts.Requests)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("reporter never received scope status")
}

func waitForPolls(t *testing.T, registry *fakeRegistry, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if registry.pollCount() >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d polls, got %d", want, registry.pollCount())
}

type recordingReporter struct {
	mu     sync.Mutex
	counts []InboxCounts
}

func (r *recordingReporter) ScopeStatus(counts InboxCounts) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts = append(r.counts, counts)
}

func (r *recordingReporter) ShareAttached(item shareclient.InboxItem) {}

func (r *recordingReporter) lastCounts() *InboxCounts {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.counts) == 0 {
		return nil
	}
	last := r.counts[len(r.counts)-1]
	return &last
}

// Compile-time interface checks against the real client types.
var (
	_ InboxRegistry = (*registryclient.Client)(nil)
	_               = fmt.Sprintf
	_               = execution.APIVersion
)
