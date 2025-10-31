package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"shardseal/pkg/repair"
)

type fakeScrubber struct {
	started bool
	stopped bool
	stats   repair.Stats
	calls   int
}

func (f *fakeScrubber) Start(ctx context.Context) error {
	f.started = true
	return nil
}
func (f *fakeScrubber) Stop(ctx context.Context) error {
	f.stopped = true
	return nil
}
func (f *fakeScrubber) RunOnce(ctx context.Context) error {
	f.calls++
	f.stats.Scanned++
	f.stats.LastRun = time.Now().UTC()
	return nil
}
func (f *fakeScrubber) Stats() repair.Stats { return f.stats }

func TestScrubberStatsHandler_GetOnly(t *testing.T) {
	fs := &fakeScrubber{}
	h := NewScrubberStatsHandler(fs)

	// GET returns JSON stats
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/scrub/stats", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET stats expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got repair.Stats
	if err := json.NewDecoder(bytes.NewReader(rr.Body.Bytes())).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Method not allowed for POST on stats
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/scrub/stats", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST stats expected 405, got %d", rr.Code)
	}
}

func TestScrubberRunOnceHandler_PostOnly(t *testing.T) {
	fs := &fakeScrubber{}
	h := NewScrubberRunOnceHandler(fs)

	// POST triggers RunOnce and returns updated stats
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/scrub/runonce", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST runonce expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got repair.Stats
	if err := json.NewDecoder(bytes.NewReader(rr.Body.Bytes())).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fs.calls != 1 {
		t.Fatalf("RunOnce calls = %d, want 1", fs.calls)
	}
	if got.Scanned == 0 {
		t.Fatalf("expected Scanned > 0 in returned stats: %+v", got)
	}
	if got.LastRun.IsZero() {
		t.Fatalf("expected LastRun set in returned stats")
	}

	// GET should be method not allowed
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/scrub/runonce", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET runonce expected 405, got %d", rr.Code)
	}
}

func TestScrubberHandlers_HandleNilScrubber(t *testing.T) {
	// Stats handler with nil scrubber should return empty stats 200
	statsH := NewScrubberStatsHandler(nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/scrub/stats", nil)
	statsH.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("nil scrubber stats expected 200, got %d", rr.Code)
	}
	var s repair.Stats
	if err := json.NewDecoder(rr.Body).Decode(&s); err != nil {
		t.Fatalf("decode nil stats: %v", err)
	}

	// RunOnce handler with nil scrubber should return 503
	runH := NewScrubberRunOnceHandler(nil)
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/scrub/runonce", nil)
	runH.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil scrubber runonce expected 503, got %d", rr.Code)
	}
}