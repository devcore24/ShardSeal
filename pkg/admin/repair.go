package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"shardseal/pkg/repair"
)

// NewRepairEnqueueHandler returns POST /admin/repair/enqueue handler.
// Body: JSON repair.RepairItem {Bucket, Key, ShardPath, Reason, Priority}.
// - Discovered is auto-set to now if zero.
// - Validation: require ShardPath OR (Bucket AND Key).
// Responses:
//  - 202 Accepted with {queued, len, item} on success
//  - 400 on invalid payload
//  - 503 if queue unavailable
//  - 409 if queue is closed
func NewRepairEnqueueHandler(q repair.RepairQueue) http.HandlerFunc {
	type enqueueResp struct {
		Queued bool              `json:"queued"`
		Len    int               `json:"len"`
		Item   repair.RepairItem `json:"item"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if q == nil {
			http.Error(w, "repair queue not configured", http.StatusServiceUnavailable)
			return
		}
		var it repair.RepairItem
		if err := json.NewDecoder(r.Body).Decode(&it); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Basic validation: either ShardPath or (Bucket and Key) must be provided.
		if it.ShardPath == "" && (it.Bucket == "" || it.Key == "") {
			http.Error(w, "missing shardPath or bucket+key", http.StatusBadRequest)
			return
		}
		if it.Discovered.IsZero() {
			it.Discovered = time.Now().UTC()
		}
		// Best-effort enqueue with request context.
		if err := q.Enqueue(r.Context(), it); err != nil {
			if errors.Is(err, repair.ErrQueueClosed) {
				http.Error(w, "repair queue closed", http.StatusConflict)
				return
			}
			http.Error(w, "enqueue failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(enqueueResp{
			Queued: true,
			Len:    q.Len(),
			Item:   it,
		})
	}
}

// NewRepairQueueStatsHandler returns GET /admin/repair/stats handler.
// Response JSON: {"len": <current queue length>}
func NewRepairQueueStatsHandler(q repair.RepairQueue) http.HandlerFunc {
	type stats struct {
		Len int `json:"len"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if q == nil {
			http.Error(w, "repair queue not configured", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(stats{Len: q.Len()})
	}
}