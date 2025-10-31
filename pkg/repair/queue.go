package repair

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// RepairItem describes a repair task detected by scrubber/read-time/administrator.
type RepairItem struct {
	Bucket     string
	Key        string
	ShardPath  string    // relative sealed shard path (e.g., objects/bkt/key/data.ss1)
	Reason     string    // "read_heal" | "scrub_fail" | "admin" | ...
	Priority   int       // reserved for future scheduling
	Discovered time.Time // when the issue was detected
}

// RepairQueue provides a minimal queue for repair jobs.
// Implementations MUST be concurrency-safe.
type RepairQueue interface {
	// Enqueue adds a repair item; returns ctx.Err() on cancellation.
	Enqueue(ctx context.Context, it RepairItem) error
	// Dequeue blocks until an item is available or the queue is closed/cancelled.
	Dequeue(ctx context.Context) (RepairItem, error)
	// Len returns a best-effort count of queued items.
	Len() int
	// Close closes the queue for new items and wakes up blocked Dequeue calls.
	Close() error
}

// ErrQueueClosed is returned when operations are attempted on a closed queue.
var ErrQueueClosed = errors.New("repairqueue: closed")

// MemQueue is an in-memory bounded queue implementation suitable for single-process workers.
// It uses a buffered channel for backpressure and supports concurrent producers/consumers.
type MemQueue struct {
	ch     chan RepairItem
	closed atomic.Bool
	once   sync.Once
}

// NewMemQueue creates a MemQueue with a bounded capacity.
// capacity <= 0 creates an unbuffered queue (may lead to producer blocking).
func NewMemQueue(capacity int) *MemQueue {
	if capacity < 0 {
		capacity = 0
	}
	return &MemQueue{ch: make(chan RepairItem, capacity)}
}

func (q *MemQueue) Enqueue(ctx context.Context, it RepairItem) error {
	if q.closed.Load() {
		return ErrQueueClosed
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case q.ch <- it:
		return nil
	}
}

func (q *MemQueue) Dequeue(ctx context.Context) (RepairItem, error) {
	select {
	case <-ctx.Done():
		return RepairItem{}, ctx.Err()
	case it, ok := <-q.ch:
		if !ok {
			return RepairItem{}, ErrQueueClosed
		}
		return it, nil
	}
}

func (q *MemQueue) Len() int {
	return len(q.ch)
}

func (q *MemQueue) Close() error {
	var err error
	q.once.Do(func() {
		q.closed.Store(true)
		close(q.ch)
	})
	if !q.closed.Load() {
		err = ErrQueueClosed
	}
	return err
}