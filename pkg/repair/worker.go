package repair

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// WorkerConfig configures the background repair worker.
// The initial implementation (M1) performs a no-op "repair" and serves as a control surface.
type WorkerConfig struct {
	// Concurrency controls the number of goroutines consuming the queue.
	// Values <= 0 default to 1.
	Concurrency int

	// Backoff applies when dequeue/process returns an error.
	// Values <= 0 default to 250ms.
	Backoff time.Duration

	// PollInterval determines how frequently a paused worker wakes up to re-check pause status.
	// Values <= 0 default to 200ms.
	PollInterval time.Duration

	// MaxRetries is reserved for future use (retrying repairs).
	MaxRetries int
}

// WorkerStats reports the worker's status and simple counters.
type WorkerStats struct {
	Running   bool      `json:"running"`
	Paused    bool      `json:"paused"`
	Processed uint64    `json:"processed"`
	Failed    uint64    `json:"failed"`
	Inflight  int       `json:"inflight"`
	QueueLen  int       `json:"queueLen"`
	LastError string    `json:"lastError"`
	Started   time.Time `json:"started"`
	Updated   time.Time `json:"updated"`
}

// Worker consumes RepairQueue items and executes repair actions.
// In this milestone it executes a no-op processor to validate control and observability paths.
type Worker struct {
	q    RepairQueue
	cfg  WorkerConfig
	wg   sync.WaitGroup
	stop context.CancelFunc

	running  atomic.Bool
	paused   atomic.Bool
	processed atomic.Uint64
	failed    atomic.Uint64
	inflight  atomic.Int32

	started time.Time

	mu       sync.Mutex
	lastErr  string
	processor func(context.Context, RepairItem) error
}

// NewWorker constructs a background worker for the given queue and config.
func NewWorker(q RepairQueue, cfg WorkerConfig) *Worker {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	return &Worker{
		q:         q,
		cfg:       cfg,
		processor: noopProcess,
	}
}

// SetProcessor allows overriding the default no-op processor.
func (w *Worker) SetProcessor(p func(context.Context, RepairItem) error) {
	if p == nil {
		p = noopProcess
	}
	w.processor = p
}

// Start launches worker goroutines that consume from the queue until Stop is called or the queue closes.
func (w *Worker) Start(ctx context.Context) error {
	if w.running.Load() {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	w.stop = cancel
	w.started = time.Now().UTC()
	w.running.Store(true)

	for i := 0; i < w.cfg.Concurrency; i++ {
		w.wg.Add(1)
		go w.loop(ctx)
	}
	return nil
}

// Stop requests shutdown and waits for worker goroutines to exit or for ctx to expire.
func (w *Worker) Stop(ctx context.Context) error {
	if !w.running.Load() {
		return nil
	}
	if w.stop != nil {
		w.stop()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.wg.Wait()
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	w.running.Store(false)
	return nil
}

// Pause prevents the worker from consuming new items until Resume is called.
func (w *Worker) Pause() { w.paused.Store(true) }

// Resume allows the worker to continue consuming items.
func (w *Worker) Resume() { w.paused.Store(false) }

// Stats returns a snapshot of the current worker status and counters.
func (w *Worker) Stats() WorkerStats {
	st := WorkerStats{
		Running:   w.running.Load(),
		Paused:    w.paused.Load(),
		Processed: w.processed.Load(),
		Failed:    w.failed.Load(),
		Inflight:  int(w.inflight.Load()),
		QueueLen:  0,
		Started:   w.started,
		Updated:   time.Now().UTC(),
	}
	if w.q != nil {
		st.QueueLen = w.q.Len()
	}
	w.mu.Lock()
	st.LastError = w.lastErr
	w.mu.Unlock()
	return st
}

// Internal loop for a single consumer goroutine.
func (w *Worker) loop(ctx context.Context) {
	defer w.wg.Done()

	poll := w.cfg.PollInterval
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		// Exit conditions
		if ctx.Err() != nil {
			return
		}
		// Pause handling: wake periodically to check status or exit.
		if w.paused.Load() {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				continue
			}
		}

		// Dequeue next item
		it, err := w.q.Dequeue(ctx)
		if err != nil {
			if errors.Is(err, ErrQueueClosed) {
				// Queue closed: exit gracefully.
				return
			}
			if ctx.Err() != nil {
				return
			}
			// Record error and back off briefly.
			w.setLastErr(err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoffOrDefault(w.cfg.Backoff, 250*time.Millisecond)):
			}
			continue
		}

		w.inflight.Add(1)
		perr := w.processor(ctx, it)
		if perr != nil {
			w.failed.Add(1)
			w.setLastErr(perr)
		} else {
			w.processed.Add(1)
		}
		w.inflight.Add(-1)
	}
}

// Default processor (M1): no-op.
func noopProcess(_ context.Context, _ RepairItem) error {
	// Placeholder for future RS reconstruction / rewrite logic.
	return nil
}

func (w *Worker) setLastErr(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	w.lastErr = err.Error()
	w.mu.Unlock()
}

func backoffOrDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}