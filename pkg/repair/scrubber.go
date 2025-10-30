package repair

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Stats captures scrubber activity metrics.
type Stats struct {
	Scanned   uint64        // number of items scanned (objects/shards) since start
	Repaired  uint64        // number of repairs performed (no-ops in noop scrubber)
	Errors    uint64        // number of errors encountered
	LastRun   time.Time     // last completed run time
	LastError string        // last error string (if any)
	Uptime    time.Duration // total time since Start
}

// Config configures the scrubber behavior.
type Config struct {
	// Interval controls periodic scrub cadence when running background.
	Interval time.Duration
	// Concurrency controls parallelism of the repair workers (no-op in noop).
	Concurrency int
}

 // Scrubber defines the background scrubber interface for ShardSeal.
 // Implementations MUST be concurrency-safe.
type Scrubber interface {
	// Start launches background scrubbing until Stop is called or context is canceled.
	Start(ctx context.Context) error
	// Stop requests the background scrubbing to stop and waits for completion (with context timeout).
	Stop(ctx context.Context) error
	// RunOnce performs a single scrub pass synchronously (may be partial or sample-based).
	RunOnce(ctx context.Context) error
	// Stats returns a snapshot of the current stats.
	Stats() Stats
}

// NoopScrubber is a no-op implementation that updates counters and sleeps on interval.
// Useful for wiring, metrics, and integration testing. NOT for production.
type NoopScrubber struct {
	cfg   Config
	mu    sync.RWMutex
	start time.Time

	running atomic.Bool
	stopCh  chan struct{}
	doneCh  chan struct{}

	scanned   atomic.Uint64
	repaired  atomic.Uint64
	errors    atomic.Uint64
	lastRun   atomic.Pointer[time.Time]
	lastError atomic.Pointer[string]
}

// NewNoopScrubber creates a no-op scrubber with sane defaults.
func NewNoopScrubber(cfg Config) *NoopScrubber {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	return &NoopScrubber{
		cfg:   cfg,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

func (n *NoopScrubber) Start(ctx context.Context) error {
	if !n.running.CompareAndSwap(false, true) {
		return errors.New("scrubber: already running")
	}
	n.mu.Lock()
	n.start = time.Now()
	n.mu.Unlock()

	go n.loop(ctx)
	return nil
}

func (n *NoopScrubber) loop(ctx context.Context) {
	defer func() {
		n.running.Store(false)
		close(n.doneCh)
	}()
	t := time.NewTimer(n.cfg.Interval)
	defer t.Stop()

	// initial run immediately
	_ = n.doRunOnce(context.Background())

	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stopCh:
			return
		case <-t.C:
			_ = n.doRunOnce(context.Background())
			t.Reset(n.cfg.Interval)
		}
	}
}

func (n *NoopScrubber) Stop(ctx context.Context) error {
	if !n.running.Load() {
		return nil
	}
	select {
	case n.stopCh <- struct{}{}:
	default:
		// already signaled
	}
	select {
	case <-n.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (n *NoopScrubber) RunOnce(ctx context.Context) error {
	return n.doRunOnce(ctx)
}

func (n *NoopScrubber) doRunOnce(_ context.Context) error {
	// Simulate a scan cycle (no real work)
	n.scanned.Add(1)

	now := time.Now()
	n.lastRun.Store(&now)
	return nil
}

func (n *NoopScrubber) Stats() Stats {
	var lastRun time.Time
	if p := n.lastRun.Load(); p != nil {
		lastRun = *p
	}
	var lastErr string
	if e := n.lastError.Load(); e != nil {
		lastErr = *e
	}

	n.mu.RLock()
	start := n.start
	n.mu.RUnlock()

	return Stats{
		Scanned:   n.scanned.Load(),
		Repaired:  n.repaired.Load(),
		Errors:    n.errors.Load(),
		LastRun:   lastRun,
		LastError: lastErr,
		Uptime:    sinceIfSet(start),
	}
}

func sinceIfSet(t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}