package listener

import (
	"context"
	"encoding/json"
	"errors"
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

// TestProducerDrainsWatchersOnReturn pins the post-return cleanup: when
// RunProducer returns (ctx cancel, watcher error, anything), in-flight
// watchers must exit so their claims free up. Otherwise stale claims block
// the next session from accepting the same Share.
func TestProducerDrainsWatchersOnReturn(t *testing.T) {
	// Use a registry with a stuck share that opens a Phoenix channel via the
	// real share client; watchShare will block in AwaitSharedRequest. The
	// mock httptest server returns no inbox items so no watcher spawns; the
	// first poll completes, then ctx is cancelled and we assert the
	// producer + drain cycle completes within the timeout.
	registry := &fakeRegistry{payload: map[string]any{
		"scopes":   []map[string]any{},
		"shares":   []map[string]any{},
		"requests": []map[string]any{},
	}}
	reg, shares := newListenerClients(t, registry)
	jobs := make(chan Job, 16)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	started := time.Now()
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
	case <-time.After(WatcherDrainTimeout + 2*time.Second):
		t.Fatal("RunProducer did not return within drain window")
	}
	if elapsed := time.Since(started); elapsed > WatcherDrainTimeout+time.Second {
		t.Fatalf("RunProducer took %s, want under %s", elapsed, WatcherDrainTimeout)
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

// TestProducerReportsTotalSharesOpen pins the pre-refactor status line:
// SharesOpen is the raw inbox.Shares count (all Shares the control plane
// considers open for this Node, regardless of whether they already carry a
// Request). The original `node up` banner used len(inbox.Shares); the
// replacement must match or the visible totals shrink and review tests of
// the banner break.
func TestProducerReportsTotalSharesOpen(t *testing.T) {
	registry := &fakeRegistry{payload: map[string]any{
		"scopes": []map[string]any{},
		"shares": []map[string]any{
			shareItem("s-awaiting", true, false), // AwaitingFulfillment=true  → SharesReady
			shareItem("s-with-req", false, true), //                              → SharesOpen
			shareItem("s-no-req", false, false),  //                              → SharesOpen
		},
		"requests": []map[string]any{},
	}}
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
			if counts.SharesOpen != 3 {
				t.Fatalf("sharesOpen = %d, want 3 (all inbox.Shares)", counts.SharesOpen)
			}
			if counts.SharesReady != 1 {
				t.Fatalf("sharesReady = %d, want 1 (awaiting fulfillment)", counts.SharesReady)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("reporter never received scope status")
}

func shareItem(id string, awaiting, hasRequest bool) map[string]any {
	return map[string]any{
		"share": map[string]any{
			"id":                 id,
			"state":              "active",
			"expiresAt":          "2030-01-01T00:00:00Z",
			"capabilityRevision": map[string]any{"name": "x", "version": "1.0.0"},
			"recipe":             nil,
			"sharedBy":           map[string]any{"id": "u", "kind": "identity"},
			"runnerTokenHash":    "h",
			"publicTokenHash":    "ph",
			"createdAt":          "2026-09-13T00:00:00Z",
		},
		"hasRequest":          hasRequest,
		"awaitingFulfillment": awaiting,
	}
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

// TestJobSinkDropsSendsAfterClose pins the shutdown safety of the Jobs
// channel: the drain is bounded, so a watcher stuck in a non-cancellable
// network call can resume after RunProducer returned. Its Job must be
// dropped, never sent on a closed channel (that panics the whole `node up`).
func TestJobSinkDropsSendsAfterClose(t *testing.T) {
	jobs := make(chan Job, 1)
	sink := &jobSink{jobs: jobs}
	if !sink.send(context.Background(), Job{Origin: "share"}) {
		t.Fatal("first send must be accepted")
	}
	<-jobs
	sink.close()
	sink.close() // idempotent: the deferred close may race a second caller
	if sink.send(context.Background(), Job{Origin: "share"}) {
		t.Fatal("send after close must be refused")
	}
	if sink.trySend(Job{Origin: "space"}) {
		t.Fatal("trySend after close must be refused")
	}
	if _, open := <-jobs; open {
		t.Fatal("Jobs must be closed for the consumer")
	}
}

// TestJobSinkReleasesBlockedSenderOnCancel pins that a watcher blocked on a
// full Jobs channel unblocks when the producer's context ends, so the drain
// can complete instead of timing out.
func TestJobSinkReleasesBlockedSenderOnCancel(t *testing.T) {
	sink := &jobSink{jobs: make(chan Job)} // unbuffered: the send blocks
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- sink.send(ctx, Job{Origin: "share"}) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case sent := <-done:
		if sent {
			t.Fatal("send reported success without a consumer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked sender never released")
	}
	sink.close()
}

// TestProducerReturnsWhileWatcherIsStuck pins the liveness half: a watcher
// blocked inside a network dial cannot hold `node up` open past the drain
// window, and the Jobs channel still closes exactly once.
func TestProducerReturnsWhileWatcherIsStuck(t *testing.T) {
	registry := &fakeRegistry{payload: map[string]any{
		"scopes": []map[string]any{},
		"shares": []map[string]any{{
			"share": map[string]any{
				"id":         "11111111-1111-4111-8111-111111111111",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
			"awaiting_fulfillment": true,
		}},
		"requests": []map[string]any{},
	}}
	reg, _ := newListenerClients(t, registry)

	dialing := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	shares, err := shareclient.New(shareclient.Options{
		Server: "http://127.0.0.1",
		Dial: func(context.Context, string) (shareclient.Socket, error) {
			once.Do(func() { close(dialing) })
			<-release
			return nil, errors.New("dial released")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer close(release)

	jobs := make(chan Job, 4)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunProducer(ctx, ProducerConfig{
			Registry:     reg,
			Shares:       shares,
			Token:        "token-1",
			PollEvery:    time.Hour,
			Jobs:         jobs,
			DrainTimeout: 100 * time.Millisecond,
		})
	}()

	<-dialing
	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("a stuck watcher held RunProducer past the drain window")
	}
	if _, open := <-jobs; open {
		t.Fatal("Jobs must be closed when RunProducer returns")
	}
}
