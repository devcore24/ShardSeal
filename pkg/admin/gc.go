package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"shardseal/pkg/metadata"
	"shardseal/pkg/storage"
)

// StagingBucket is the internal bucket where multipart parts are staged.
const StagingBucket = ".multipart"

// UploadStore is the subset of the metadata store needed for multipart GC.
type UploadStore interface {
	ListMultipartUploads(ctx context.Context, bucket string) ([]metadata.MultipartUpload, error)
	AbortMultipartUpload(ctx context.Context, uploadID string) error
}

// ObjectStore is the subset of the object storage needed for multipart GC.
type ObjectStore interface {
	List(ctx context.Context, bucket, prefix, startAfter string, maxKeys int) ([]storage.ObjectMeta, bool, error)
	Delete(ctx context.Context, bucket, key string) error
}

// Stats summarizes a GC pass.
type Stats struct {
	Scanned int `json:"scanned"`
	Aborted int `json:"aborted"`
	Deleted int `json:"deletedParts"`
}

// RunMultipartGC performs a best-effort garbage collection for stale multipart uploads.
// It deletes staged parts older than 'olderThan' from the internal staging bucket
// and aborts the corresponding uploads in metadata.
func RunMultipartGC(ctx context.Context, store UploadStore, objs ObjectStore, olderThan time.Duration) (Stats, error) {
	var res Stats
	uploads, err := store.ListMultipartUploads(ctx, "")
	if err != nil {
		return res, err
	}
	now := time.Now().UTC()

	for _, up := range uploads {
		res.Scanned++
		// Skip uploads newer than threshold
		if now.Sub(up.Initiated) < olderThan {
			continue
		}
		// Delete staged parts under prefix: "<bucket>/<key>/<uploadID>/"
		prefix := up.Bucket + "/" + up.Key + "/" + up.UploadID + "/"
		startAfter := ""
		for {
			objsList, truncated, lerr := objs.List(ctx, StagingBucket, prefix, startAfter, 1000)
			if lerr != nil {
				// best-effort: continue with next upload
				slog.Error("admin gc: list parts", slog.String("uploadId", up.UploadID), slog.String("error", lerr.Error()))
				break
			}
			if len(objsList) == 0 && !truncated {
				break
			}
			for _, o := range objsList {
				if derr := objs.Delete(ctx, StagingBucket, o.Key); derr == nil {
					res.Deleted++
				} else {
					slog.Error("admin gc: delete part", slog.String("uploadId", up.UploadID), slog.String("key", o.Key), slog.String("error", derr.Error()))
				}
				startAfter = o.Key
			}
			if !truncated {
				break
			}
		}
		// Abort upload in metadata (ignore error if already gone)
		if err := store.AbortMultipartUpload(ctx, up.UploadID); err == nil {
			res.Aborted++
		}
	}
	return res, nil
}

// NewMultipartGCHandler returns POST /admin/gc/multipart handler that accepts ?olderThan=24h
// and runs a single GC pass. Method must be POST; returns JSON Stats.
func NewMultipartGCHandler(store UploadStore, objs ObjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		olderThan := 24 * time.Hour
		if qs := r.URL.Query().Get("olderThan"); qs != "" {
			if d, err := time.ParseDuration(qs); err == nil && d > 0 {
				olderThan = d
			}
		}
		res, err := RunMultipartGC(r.Context(), store, objs, olderThan)
		if err != nil {
			http.Error(w, "failed to run gc: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(res)
	}
}

// StartMultipartGC launches a periodic background GC that runs RunMultipartGC at the given interval.
// Returns a stop function to cancel the loop. If interval/olderThan are invalid, safe defaults are applied.
// Logs a summary after each pass. Safe for use from main and tests.
func StartMultipartGC(parent context.Context, store UploadStore, objs ObjectStore, interval, olderThan time.Duration, logger *slog.Logger) context.CancelFunc {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	if olderThan <= 0 {
		olderThan = 24 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				res, err := RunMultipartGC(context.Background(), store, objs, olderThan)
				if err != nil {
					logger.Error("gc: multipart run failed", slog.String("error", err.Error()))
					continue
				}
				logger.Info("gc: multipart pass",
					slog.Int("scanned", res.Scanned),
					slog.Int("aborted", res.Aborted),
					slog.Int("deleted", res.Deleted),
					slog.String("olderThan", olderThan.String()),
				)
			}
		}
	}()
	return cancel
}