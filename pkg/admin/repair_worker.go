package admin

import (
	"encoding/json"
	"net/http"

	"shardseal/pkg/repair"
)

// NewRepairWorkerStatsHandler returns GET /admin/repair/worker/stats.
// Response: JSON repair.WorkerStats. 503 when worker not configured.
func NewRepairWorkerStatsHandler(wrk *repair.Worker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if wrk == nil {
			http.Error(w, "repair worker not configured", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(wrk.Stats())
	}
}

// NewRepairWorkerPauseHandler returns POST /admin/repair/worker/pause.
// Action: pause the worker; Response: JSON repair.WorkerStats. 503 when worker not configured.
func NewRepairWorkerPauseHandler(wrk *repair.Worker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if wrk == nil {
			http.Error(w, "repair worker not configured", http.StatusServiceUnavailable)
			return
		}
		wrk.Pause()
		_ = json.NewEncoder(w).Encode(wrk.Stats())
	}
}

// NewRepairWorkerResumeHandler returns POST /admin/repair/worker/resume.
// Action: resume the worker; Response: JSON repair.WorkerStats. 503 when worker not configured.
func NewRepairWorkerResumeHandler(wrk *repair.Worker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if wrk == nil {
			http.Error(w, "repair worker not configured", http.StatusServiceUnavailable)
			return
		}
		wrk.Resume()
		_ = json.NewEncoder(w).Encode(wrk.Stats())
	}
}