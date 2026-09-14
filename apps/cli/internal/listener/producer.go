// Package listener hosts the headless orchestration behind `doppels node
// up`: polling the Cloud inbox for Requests, attaching to Shares that are
// waiting for fulfillment, and queueing Jobs for a consumer to decide.
//
// The package deliberately has no TTY, flag, or exit-code knowledge. A CLI
// consumer wires presentation through Reporter; a future Runner daemon
// wires IPC subscribers instead. Fulfillment decisions surface through
// ApprovalPort so stdin prompting never leaks into orchestration.
package listener

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/manifest"
	"doppels.so/cli/internal/registryclient"
	"doppels.so/cli/internal/shareclient"
)

// Job is one inbound unit of work: either a Space Request polled from the
// registry inbox (Origin "space") or a Share whose recipient just submitted
// inputs (Origin "share").
type Job struct {
	Origin       string // "share" or "space"
	Created      *shareclient.ShareCreated
	Channel      *shareclient.Channel
	Request      *execution.RequestRecord
	Organization string
	Space        string
}

// Decision is the operator's answer to an inbound Job.
type Decision int

const (
	DecisionApprove Decision = iota
	DecisionReject
	DecisionSkip
	DecisionBackground
)

// QueueInfo describes the position of the Job being decided within the
// current backlog, so prompt surfaces can render "[2/3]"-style context.
type QueueInfo struct {
	Index  int
	Total  int
	Queued []string
}

// ApprovalPort is the headless replacement for stdin interaction: every
// place `node up` used to block on the terminal, orchestration now asks a
// port. The CLI implements it with TTY prompts; the Runner daemon will
// implement it with pending-approval IPC requests.
type ApprovalPort interface {
	// DecideFulfillment resolves what to do with an inbound Job.
	DecideFulfillment(ctx context.Context, job Job, queue QueueInfo) (Decision, error)
	// ApproveStep decides a required-Step approval mid-run.
	ApproveStep(ctx context.Context, request execution.ApprovalRequest) (bool, error)
	// PickRecipe disambiguates multiple local Recipes providing one
	// Capability.
	PickRecipe(ctx context.Context, capability string, matches []manifest.RecipeDefinition) (manifest.RecipeDefinition, error)
	// FulfillManual completes a manual-recipe run.
	FulfillManual(ctx context.Context, request execution.ManualRequest) (execution.ManualResult, error)
}

// Filters narrows what the Node serves, mirroring `node up` flags.
type Filters struct {
	Organization string
	Space        string
	Capability   string
}

// InboxCounts carries the poll-derived numbers a status line needs.
type InboxCounts struct {
	Scopes     int
	SharesOpen int
	// SharesReady counts Shares with a Request awaiting fulfillment.
	SharesReady int
	// Requests counts every Space Request the inbox reported, actionable
	// or not.
	Requests int
}

// Reporter receives presentation-only callbacks. Implementations own
// formatting, pausing, and JSON-vs-TTY selection; nil disables reporting.
type Reporter interface {
	ScopeStatus(counts InboxCounts)
	ShareAttached(item shareclient.InboxItem)
}

// InboxRegistry is the polling seam; *registryclient.Client satisfies it.
type InboxRegistry interface {
	ListenInbox(ctx context.Context, token string, filters registryclient.ListenFilters) (*registryclient.ListenInbox, error)
}

// ProducerConfig configures the inbox producer loop.
type ProducerConfig struct {
	Registry  InboxRegistry
	Shares    *shareclient.Client
	Token     string
	Filters   Filters
	PollEvery time.Duration
	Jobs      chan<- Job
	Reporter  Reporter
	// DrainTimeout bounds how long RunProducer waits for in-flight watchers
	// before returning (default WatcherDrainTimeout).
	DrainTimeout time.Duration
}

// jobSink owns cfg.Jobs. Watchers can outlive the bounded drain (a stuck
// network call is not always cancellable), and a watcher that resumes after
// RunProducer returned must drop its Job, not panic the process with a send
// on a closed channel.
type jobSink struct {
	mu     sync.RWMutex
	jobs   chan<- Job
	closed bool
}

// send blocks until the Job is queued, the context ends, or the sink closes.
func (s *jobSink) send(ctx context.Context, job Job) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	select {
	case s.jobs <- job:
		return true
	case <-ctx.Done():
		return false
	}
}

// trySend queues a Job only if the consumer has room right now.
func (s *jobSink) trySend(job Job) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	select {
	case s.jobs <- job:
		return true
	default:
		return false
	}
}

func (s *jobSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.jobs)
}

// RunProducer polls the registry inbox and watches claimed Shares, feeding
// Jobs to cfg.Jobs. It performs an immediate first poll, then ticks.
//
// Contract:
//   - cfg.Jobs is closed when RunProducer returns.
//   - A Share or Request is claimed at most once at a time; skipped or
//     unwatched items are released for the next poll.
//   - Watcher errors abort the producer (returned) so the consumer decides
//     whether the Node stays online.
//   - On return, RunProducer waits for in-flight share watchers to release
//     their claims (capped by cfg.DrainTimeout, default WatcherDrainTimeout).
//     This guarantees no orphan claims survive a normal shutdown or an
//     explicit error path, since the watchers run on a cancellable context
//     derived from ctx.
//   - The drain is bounded on purpose: a watcher stuck in a network call
//     that ignores cancellation must not keep `node up` alive forever. Such
//     a watcher may therefore resume after RunProducer returned, so every
//     send goes through jobSink and is dropped once cfg.Jobs is closed
//     instead of panicking the process.
func RunProducer(ctx context.Context, cfg ProducerConfig) error {
	var watchers sync.WaitGroup
	drainTimeout := cfg.DrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = WatcherDrainTimeout
	}
	sink := &jobSink{jobs: cfg.Jobs}
	watcherCtx, watcherCancel := context.WithCancel(ctx)
	defer func() {
		watcherCancel()
		drainWatchers(&watchers, drainTimeout)
		sink.close()
	}()

	var mu sync.Mutex
	claimedShares := map[string]bool{}
	claimedRequests := map[string]bool{}

	claimShare := func(shareID string) bool {
		mu.Lock()
		defer mu.Unlock()
		if claimedShares[shareID] {
			return false
		}
		claimedShares[shareID] = true
		return true
	}
	releaseShare := func(shareID string) {
		mu.Lock()
		delete(claimedShares, shareID)
		mu.Unlock()
	}
	claimRequest := func(requestID string) bool {
		mu.Lock()
		defer mu.Unlock()
		if claimedRequests[requestID] {
			return false
		}
		claimedRequests[requestID] = true
		return true
	}
	releaseRequest := func(requestID string) {
		mu.Lock()
		delete(claimedRequests, requestID)
		mu.Unlock()
	}

	watcherErr := make(chan error, 1)

	poll := func() error {
		if cfg.Registry == nil {
			return errors.New("registry client is required for node up")
		}
		inbox, err := cfg.Registry.ListenInbox(ctx, cfg.Token, registryclient.ListenFilters{
			Organization: cfg.Filters.Organization,
			Space:        cfg.Filters.Space,
			Capability:   cfg.Filters.Capability,
		})
		if err != nil {
			return err
		}
		counts := InboxCounts{Scopes: len(inbox.Scopes), SharesOpen: len(inbox.Shares), Requests: len(inbox.Requests)}
		for _, item := range inbox.Shares {
			if item.AwaitingFulfillment {
				counts.SharesReady++
			}
		}
		if cfg.Reporter != nil {
			cfg.Reporter.ScopeStatus(counts)
		}
		for _, item := range inbox.Shares {
			if !item.AwaitingFulfillment && item.HasRequest {
				continue
			}
			if !claimShare(item.Share.ID) {
				continue
			}
			shareID := item.Share.ID
			watchers.Add(1)
			go func(item shareclient.InboxItem) {
				defer watchers.Done()
				handedOff, err := watchShare(watcherCtx, cfg, sink, item)
				if err != nil {
					releaseShare(shareID)
					if watcherCtx.Err() == nil {
						select {
						case watcherErr <- err:
						default:
						}
					}
					return
				}
				if !handedOff {
					releaseShare(shareID)
				}
			}(shareclient.InboxItem(item))
		}
		for _, pending := range inbox.Requests {
			if pending.Status != "requested" && pending.Status != "approved" {
				continue
			}
			if !claimRequest(pending.Summary.ID) {
				continue
			}
			requestID := pending.Summary.ID
			org := pending.Summary.Organization
			space := pending.Summary.Space
			if org == "" || space == "" {
				releaseRequest(requestID)
				continue
			}
			request := pending.Summary
			select {
			case <-ctx.Done():
				releaseRequest(requestID)
				return ctx.Err()
			default:
			}
			if !sink.trySend(Job{
				Origin:       "space",
				Request:      &request,
				Organization: org,
				Space:        space,
			}) {
				releaseRequest(requestID)
			}
		}
		return nil
	}

	if err := poll(); err != nil {
		return err
	}

	ticker := time.NewTicker(cfg.PollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-watcherErr:
			return err
		case <-ticker.C:
			if err := poll(); err != nil {
				return err
			}
		}
	}
}

// drainWatchers blocks until every spawned watcher exits, then cancels any
// still-pending context so the connection's read deadline fires even when
// the watcher is stuck in a synchronous channel call. Bounded so a stalled
// watcher cannot keep the producer alive past its return path.
func drainWatchers(watchers *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() { watchers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// WatcherDrainTimeout bounds how long RunProducer waits for in-flight
// watchers after returning. After this it cancels the watcher context
// unconditionally; the channel goroutines unblock on the next read.
const WatcherDrainTimeout = 5 * time.Second

// watchShare attaches to one Share and waits for its recipient to submit a
// Request. handedOff reports whether the channel was handed to a consumer
// via cfg.Jobs (the consumer then owns closing it); otherwise the channel is
// closed before returning.
func watchShare(ctx context.Context, cfg ProducerConfig, sink *jobSink, item shareclient.InboxItem) (handedOff bool, err error) {
	created := &shareclient.ShareCreated{
		Share:       item.Share,
		RunnerToken: cfg.Token,
	}
	channel, initial, err := cfg.Shares.OpenChannel(ctx, item.Share.ID, cfg.Token)
	if err != nil {
		return false, fmt.Errorf("connect Share %s: %w", item.Share.ID, err)
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			channel.Close()
		}
	}()

	if cfg.Reporter != nil {
		cfg.Reporter.ShareAttached(item)
	}

	waitContext, cancelWait := context.WithDeadline(ctx, item.Share.ExpiresAt)
	defer cancelWait()
	request, alreadyComplete, err := AwaitSharedRequest(waitContext, cfg.Shares, channel, created, initial)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return false, nil
		}
		return false, err
	}
	if alreadyComplete || request == nil {
		return false, nil
	}

	if !sink.send(ctx, Job{Origin: "share", Created: created, Channel: channel, Request: request}) {
		// The consumer is gone (cancelled or already drained): the deferred
		// close releases the channel and the caller releases the claim.
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return false, nil
	}
	keepOpen = true
	return true, nil
}
