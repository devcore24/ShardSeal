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

// Helpers

func doRequest(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		buf = bytes.NewBuffer(b)
	} else {
		buf = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// Tests for NewRepairQueueStatsHandler

func TestRepairQueueStatsHandler_MethodNotAllowed(t *testing.T) {
	q := repair.NewMemQueue(1)
	h := NewRepairQueueStatsHandler(q)

	rr := doRequest(t, h, http.MethodPost, "/admin/repair/stats", nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestRepairQueueStatsHandler_NilQueue(t *testing.T) {
	h := NewRepairQueueStatsHandler(nil)

	rr := doRequest(t, h, http.MethodGet, "/admin/repair/stats", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepairQueueStatsHandler_OK(t *testing.T) {
	q := repair.NewMemQueue(10)
	ctx := context.Background()
	_ = q.Enqueue(ctx, repair.RepairItem{ShardPath: "a", Reason: "admin", Discovered: time.Now().UTC()})
	_ = q.Enqueue(ctx, repair.RepairItem{ShardPath: "b", Reason: "admin", Discovered: time.Now().UTC()})

	h := NewRepairQueueStatsHandler(q)
	rr := doRequest(t, h, http.MethodGet, "/admin/repair/stats", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", rr.Code, rr.Body.String())
	}
	var res struct {
		Len int `json:"len"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.Len != 2 {
		t.Fatalf("expected len=2, got %d", res.Len)
	}
}

// Tests for NewRepairEnqueueHandler

func TestRepairEnqueueHandler_MethodNotAllowed(t *testing.T) {
	q := repair.NewMemQueue(1)
	h := NewRepairEnqueueHandler(q)

	rr := doRequest(t, h, http.MethodGet, "/admin/repair/enqueue", nil)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestRepairEnqueueHandler_InvalidJSON(t *testing.T) {
	q := repair.NewMemQueue(1)
	h := NewRepairEnqueueHandler(q)

	req := httptest.NewRequest(http.MethodPost, "/admin/repair/enqueue", bytes.NewBufferString("{"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestRepairEnqueueHandler_MissingFields(t *testing.T) {
	q := repair.NewMemQueue(1)
	h := NewRepairEnqueueHandler(q)

	// Missing ShardPath and missing Bucket/Key
	payload := map[string]any{}
	rr := doRequest(t, h, http.MethodPost, "/admin/repair/enqueue", payload)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepairEnqueueHandler_OK_WithShardPath(t *testing.T) {
	q := repair.NewMemQueue(10)
	h := NewRepairEnqueueHandler(q)

	payload := repair.RepairItem{
		ShardPath: "objects/bkt/key/data.ss1",
		Reason:    "admin",
		// Discovered intentionally zero to test auto-fill
	}

	rr := doRequest(t, h, http.MethodPost, "/admin/repair/enqueue", payload)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d; body=%s", rr.Code, rr.Body.String())
	}

	var res struct {
		Queued bool              `json:"queued"`
		Len    int               `json:"len"`
		Item   repair.RepairItem `json:"item"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !res.Queued {
		t.Fatalf("expected queued=true")
	}
	if res.Len != 1 {
		t.Fatalf("expected len=1, got %d", res.Len)
	}
	if res.Item.ShardPath != payload.ShardPath || res.Item.Reason != payload.Reason {
		t.Fatalf("unexpected item in response: %+v", res.Item)
	}
	if res.Item.Discovered.IsZero() {
		t.Fatalf("expected Discovered to be set")
	}
}

func TestRepairEnqueueHandler_OK_WithBucketKey(t *testing.T) {
	q := repair.NewMemQueue(10)
	h := NewRepairEnqueueHandler(q)

	payload := repair.RepairItem{
		Bucket: "bkt",
		Key:    "key",
		Reason: "admin",
	}

	rr := doRequest(t, h, http.MethodPost, "/admin/repair/enqueue", payload)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d; body=%s", rr.Code, rr.Body.String())
	}

	var res struct {
		Queued bool              `json:"queued"`
		Len    int               `json:"len"`
		Item   repair.RepairItem `json:"item"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !res.Queued || res.Len != 1 {
		t.Fatalf("expected queued=true len=1, got queued=%v len=%d", res.Queued, res.Len)
	}
	if res.Item.Bucket != "bkt" || res.Item.Key != "key" {
		t.Fatalf("unexpected item fields: %+v", res.Item)
	}
}

func TestRepairEnqueueHandler_ClosedQueue(t *testing.T) {
	q := repair.NewMemQueue(1)
	_ = q.Close()
	h := NewRepairEnqueueHandler(q)

	payload := repair.RepairItem{
		ShardPath: "objects/bkt/key/data.ss1",
		Reason:    "admin",
	}

	rr := doRequest(t, h, http.MethodPost, "/admin/repair/enqueue", payload)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d; body=%s", rr.Code, rr.Body.String())
	}
}