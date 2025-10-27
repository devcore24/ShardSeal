package s3

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"os"
	"context"
	"crypto/md5"
	"encoding/hex"
	"strings"

	"s3bee/pkg/metadata"
)

// stub object store that keeps objects in memory for fast tests

type memObj struct {
	data []byte
	etag string
}

type memStore struct {
	objs map[string]memObj
}

func newMemStore() *memStore { return &memStore{objs: map[string]memObj{}} }

func (m *memStore) Put(_ context.Context, bucket, key string, r io.Reader) (string, int64, error) {
	b, _ := io.ReadAll(r)
	etag := md5hex(b)
	m.objs[bucket+"/"+key] = memObj{data: b, etag: etag}
	return etag, int64(len(b)), nil
}
func (m *memStore) Get(_ context.Context, bucket, key string) (io.ReadCloser, int64, string, time.Time, error) {
	o, ok := m.objs[bucket+"/"+key]
	if !ok { return nil, 0, "", time.Time{}, os.ErrNotExist }
	return io.NopCloser(bytes.NewReader(o.data)), int64(len(o.data)), o.etag, time.Now().UTC(), nil
}
func (m *memStore) Head(_ context.Context, bucket, key string) (int64, string, time.Time, error) {
	o, ok := m.objs[bucket+"/"+key]
	if !ok { return 0, "", time.Time{}, os.ErrNotExist }
	return int64(len(o.data)), o.etag, time.Now().UTC(), nil
}
func (m *memStore) Delete(_ context.Context, bucket, key string) error {
	k := bucket+"/"+key
	if _, ok := m.objs[k]; !ok { return os.ErrNotExist }
	delete(m.objs, k)
	return nil
}
func (m *memStore) IsBucketEmpty(_ context.Context, bucket string) (bool, error) {
	prefix := bucket+"/"
	for k := range m.objs { if strings.HasPrefix(k, prefix) { return false, nil } }
	return true, nil
}
func (m *memStore) RemoveBucket(_ context.Context, bucket string) error { return nil }

func TestBuckets_ListAndCreate(t *testing.T) {
	meta := metadata.NewMemoryStore()
	objs := newMemStore()
	s := New(meta, objs)
	hs := s.Handler()

	// List should be empty
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }

	// Create bucket
	r = httptest.NewRequest(http.MethodPut, "/my-bucket", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("create bucket expected 200, got %d", w.Code) }

	// List should include bucket name
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	if !bytes.Contains(w.Body.Bytes(), []byte("my-bucket")) { t.Fatalf("expected my-bucket in list: %s", w.Body.String()) }
}

func TestObjects_PutGetDelete(t *testing.T) {
	meta := metadata.NewMemoryStore()
	_ = meta.CreateBucket(context.Background(), "b")
	objs := newMemStore()
	s := New(meta, objs)
	hs := s.Handler()

	// Put object
	body := bytes.NewBufferString("hello")
	r := httptest.NewRequest(http.MethodPut, "/b/hello.txt", body)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("put expected 200, got %d", w.Code) }
	if w.Header().Get("ETag") == "" { t.Fatalf("expected ETag header") }

	// Get object
	r = httptest.NewRequest(http.MethodGet, "/b/hello.txt", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("get expected 200, got %d", w.Code) }
	if got := w.Body.String(); got != "hello" { t.Fatalf("unexpected body: %q", got) }

	// Delete object
	r = httptest.NewRequest(http.MethodDelete, "/b/hello.txt", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 204 { t.Fatalf("delete expected 204, got %d", w.Code) }

	// Get should 404
	r = httptest.NewRequest(http.MethodGet, "/b/hello.txt", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 404 { t.Fatalf("expected 404 after delete, got %d", w.Code) }
}

func TestBucket_DeleteLifecycle(t *testing.T) {
	meta := metadata.NewMemoryStore()
	_ = meta.CreateBucket(context.Background(), "bkt")
	objs := newMemStore()
	// put one object
	etag, _, _ := objs.Put(context.Background(), "bkt", "x", bytes.NewBufferString("x"))
	_ = etag
	s := New(meta, objs)
	hs := s.Handler()
	// delete bucket should 409
	r := httptest.NewRequest(http.MethodDelete, "/bkt", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 409 { t.Fatalf("delete bucket expected 409, got %d", w.Code) }
	// delete object then delete bucket
	r = httptest.NewRequest(http.MethodDelete, "/bkt/x", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 204 { t.Fatalf("delete object expected 204, got %d", w.Code) }
	r = httptest.NewRequest(http.MethodDelete, "/bkt", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 204 { t.Fatalf("delete bucket expected 204, got %d", w.Code) }
}

// helpers

func md5hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}
