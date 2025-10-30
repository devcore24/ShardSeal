package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"s3free/pkg/metadata"
	"s3free/pkg/storage"
)

// ---- fakes ----

type fakeUploadStore struct {
	uploads []metadata.MultipartUpload
	aborted map[string]bool
}

func (f *fakeUploadStore) ListMultipartUploads(ctx context.Context, bucket string) ([]metadata.MultipartUpload, error) {
	// return a copy to mimic real store behavior
	out := make([]metadata.MultipartUpload, len(f.uploads))
	copy(out, f.uploads)
	return out, nil
}

func (f *fakeUploadStore) AbortMultipartUpload(ctx context.Context, uploadID string) error {
	if f.aborted == nil {
		f.aborted = map[string]bool{}
	}
	f.aborted[uploadID] = true
	return nil
}

type fakeObjectStore struct {
	// bucket -> list of objects (Key only used)
	objects map[string][]storage.ObjectMeta
	deleted []string
}

func (f *fakeObjectStore) List(ctx context.Context, bucket, prefix, startAfter string, maxKeys int) ([]storage.ObjectMeta, bool, error) {
	var out []storage.ObjectMeta
	if f.objects != nil {
		for _, o := range f.objects[bucket] {
			if len(prefix) == 0 || (len(o.Key) >= len(prefix) && o.Key[:len(prefix)] == prefix) {
				// startAfter semantics: return only keys strictly greater
				if startAfter == "" || o.Key > startAfter {
					out = append(out, o)
				}
			}
		}
	}
	// Simplify: never truncate in fake
	return out, false, nil
}

func (f *fakeObjectStore) Delete(ctx context.Context, bucket, key string) error {
	f.deleted = append(f.deleted, key)
	// Remove from in-memory list for idempotence in tests
	if list, ok := f.objects[bucket]; ok {
		n := list[:0]
		for _, o := range list {
			if o.Key != key {
				n = append(n, o)
			}
		}
		f.objects[bucket] = n
	}
	return nil
}

// ---- tests ----

func TestRunMultipartGC_Basic(t *testing.T) {
	old := time.Now().UTC().Add(-48 * time.Hour)
	yng := time.Now().UTC().Add(-1 * time.Hour)

	upOld := metadata.MultipartUpload{
		UploadID:  "u-old",
		Bucket:    "b",
		Key:       "k",
		Initiated: old,
		Parts:     map[int]metadata.Part{1: {PartNumber: 1}, 2: {PartNumber: 2}},
	}
	upYoung := metadata.MultipartUpload{
		UploadID:  "u-young",
		Bucket:    "b",
		Key:       "k2",
		Initiated: yng,
		Parts:     map[int]metadata.Part{1: {PartNumber: 1}},
	}

	store := &fakeUploadStore{
		uploads: []metadata.MultipartUpload{upOld, upYoung},
		aborted: map[string]bool{},
	}

	objs := &fakeObjectStore{
		objects: map[string][]storage.ObjectMeta{
			StagingBucket: {
				{Key: "b/k/u-old/part.1"},
				{Key: "b/k/u-old/part.2"},
				{Key: "b/k2/u-young/part.1"},
			},
		},
	}

	stats, err := RunMultipartGC(context.Background(), store, objs, 24*time.Hour)
	if err != nil {
		t.Fatalf("RunMultipartGC error: %v", err)
	}

	// Should scan 2 uploads
	if stats.Scanned != 2 {
		t.Fatalf("Scanned=%d, want 2", stats.Scanned)
	}
	// Should abort only the old upload
	if !store.aborted["u-old"] {
		t.Fatalf("expected u-old aborted")
	}
	if store.aborted["u-young"] {
		t.Fatalf("did not expect u-young aborted")
	}
	// Should delete only old parts (2)
	if stats.Deleted != 2 {
		t.Fatalf("Deleted=%d, want 2", stats.Deleted)
	}
	// Ensure staging parts for young upload still present
	var stillYoung bool
	for _, o := range objs.objects[StagingBucket] {
		if o.Key == "b/k2/u-young/part.1" {
			stillYoung = true
		}
	}
	if !stillYoung {
		t.Fatalf("young upload part unexpectedly deleted")
	}
}

func TestNewMultipartGCHandler_Ok(t *testing.T) {
	old := time.Now().UTC().Add(-72 * time.Hour)

	upOld := metadata.MultipartUpload{
		UploadID:  "u-old",
		Bucket:    "bx",
		Key:       "kx",
		Initiated: old,
		Parts:     map[int]metadata.Part{1: {PartNumber: 1}},
	}

	store := &fakeUploadStore{
		uploads: []metadata.MultipartUpload{upOld},
		aborted: map[string]bool{},
	}
	objs := &fakeObjectStore{
		objects: map[string][]storage.ObjectMeta{
			StagingBucket: {
				{Key: "bx/kx/u-old/part.1"},
			},
		},
	}
	h := NewMultipartGCHandler(store, objs)

	req := httptest.NewRequest(http.MethodPost, "/admin/gc/multipart?olderThan=24h", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	// parse JSON
	var got Stats
	if err := json.NewDecoder(bytes.NewReader(rr.Body.Bytes())).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Aborted != 1 || got.Deleted != 1 || got.Scanned != 1 {
		t.Fatalf("stats=%+v, want Aborted=1 Deleted=1 Scanned=1", got)
	}
	if !store.aborted["u-old"] {
		t.Fatalf("expected u-old aborted")
	}
}