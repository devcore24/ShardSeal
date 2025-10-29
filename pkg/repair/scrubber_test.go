package repair

import (
	"context"
	"testing"
	"time"
)

func TestNoopScrubber_RunOnce(t *testing.T) {
	s := NewNoopScrubber(Config{Interval: 5 * time.Millisecond})
	before := s.Stats().Scanned
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	after := s.Stats().Scanned
	if after != before+1 {
		t.Fatalf("expected scanned to increase by 1, got before=%d after=%d", before, after)
	}
	if s.Stats().LastRun.IsZero() {
		t.Fatalf("expected LastRun to be set")
	}
}

func TestNoopScrubber_StartStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	s := NewNoopScrubber(Config{Interval: 10 * time.Millisecond})
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Allow at least one loop tick
	time.Sleep(25 * time.Millisecond)

	// Stop with reasonable timeout
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stopCancel()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Stats sanity
	st := s.Stats()
	if st.Uptime <= 0 {
		t.Fatalf("expected uptime > 0")
	}
	// Should have at least one scan from loop + possibly more
	if st.Scanned == 0 {
		t.Fatalf("expected scanned > 0, got %d", st.Scanned)
	}
}