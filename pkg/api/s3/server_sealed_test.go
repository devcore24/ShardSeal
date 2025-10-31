package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shardseal/pkg/erasure"
	"shardseal/pkg/metadata"
	"shardseal/pkg/storage"
)

func md5hexS3(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

func TestS3_Sealed_PutGetHeadRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// LocalFS with sealed mode enabled (no verify-on-read for performance)
	base := t.TempDir()
	lfs, err := storage.NewLocalFS([]string{base})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	lfs.SetSealed(true, false)

	// Metadata store and bucket
	meta := metadata.NewMemoryStore()
	const bucket = "bkt"
	if err := meta.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// S3 API with LocalFS
	s := New(meta, lfs)
	hs := s.Handler()

	// Put object via S3 API
	const key = "dir/obj.txt"
	payload := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
	r := httptest.NewRequest(http.MethodPut, "/"+bucket+"/"+key, bytes.NewReader(payload))
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}
	wantETag := md5hexS3(payload)
	if got := w.Header().Get("ETag"); got != "\""+wantETag+"\"" {
		t.Fatalf("ETag mismatch: got=%q want=%q", got, "\""+wantETag+"\"")
	}

	// HEAD returns size/etag/last-modified
	r = httptest.NewRequest(http.MethodHead, "/"+bucket+"/"+key, nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("ETag"); got != "\""+wantETag+"\"" {
		t.Fatalf("HEAD ETag mismatch: got=%q want=%q", got, "\""+wantETag+"\"")
	}
	if got := w.Header().Get("Content-Length"); got != "62" {
		t.Fatalf("HEAD Content-Length mismatch: got=%q want=62", got)
	}
	if lm := w.Header().Get("Last-Modified"); lm == "" {
		t.Fatalf("HEAD missing Last-Modified")
	}

	// GET full
	r = httptest.NewRequest(http.MethodGet, "/"+bucket+"/"+key, nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Fatalf("GET body mismatch")
	}

	// Range GET: bytes=10-25
	r = httptest.NewRequest(http.MethodGet, "/"+bucket+"/"+key, nil)
	r.Header.Set("Range", "bytes=10-25")
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("Range GET expected 206, got %d: %s", w.Code, w.Body.String())
	}
	exp := payload[10 : 25+1]
	if !bytes.Equal(w.Body.Bytes(), exp) {
		t.Fatalf("Range body mismatch: got=%q want=%q", w.Body.String(), string(exp))
	}
}

func TestS3_Sealed_IntegrityErrorMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// LocalFS with sealed + verify-on-read
	tmp := t.TempDir()
	lfs, err := storage.NewLocalFS([]string{tmp})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	lfs.SetSealed(true, true)

	// Metadata store and bucket
	meta := metadata.NewMemoryStore()
	const bucket = "bkt"
	if err := meta.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// S3 API
	s := New(meta, lfs)
	hs := s.Handler()

	// PUT object
	const key = "obj.bin"
	data := bytes.Repeat([]byte("X"), 4096)
	r := httptest.NewRequest(http.MethodPut, "/"+bucket+"/"+key, bytes.NewReader(data))
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Corrupt the sealed footer to trigger integrity error
	absBase, _ := filepath.Abs(tmp)
	shard := filepath.Join(absBase, "objects", bucket, filepath.FromSlash(key), "data.ss1")
	f, err := os.OpenFile(shard, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open shard: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	hdr, err := erasure.DecodeShardHeader(f)
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	footerOff := int64(hdr.HeaderSize) + int64(hdr.PayloadLength)
	if _, err := f.Seek(footerOff, io.SeekStart); err != nil {
		t.Fatalf("seek footer: %v", err)
	}
	foot := make([]byte, 36) // 32 sha256 + 4 crc
	if _, err := io.ReadFull(f, foot); err != nil {
		t.Fatalf("read footer: %v", err)
	}
	foot[0] ^= 0xFF // flip a bit in content hash
	if _, err := f.Seek(footerOff, io.SeekStart); err != nil {
		t.Fatalf("seek back: %v", err)
	}
	if _, err := f.Write(foot); err != nil {
		t.Fatalf("write footer: %v", err)
	}
	_ = f.Close()

	// GET should map integrity error to InternalError (500)
	r = httptest.NewRequest(http.MethodGet, "/"+bucket+"/"+key, nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("GET expected 500 on integrity failure, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-S3-Error-Code"); got != "InternalError" {
		t.Fatalf("expected X-S3-Error-Code=InternalError, got %q", got)
	}

	// HEAD should also error with 500 when verify-on-read is on
	r = httptest.NewRequest(http.MethodHead, "/"+bucket+"/"+key, nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("HEAD expected 500 on integrity failure, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-S3-Error-Code"); got != "InternalError" {
		t.Fatalf("expected X-S3-Error-Code=InternalError (HEAD), got %q", got)
	}
}

func TestS3_Sealed_ListObjectsV2_WithSealedObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Sealed LocalFS (no verify for list)
	tmp := t.TempDir()
	lfs, err := storage.NewLocalFS([]string{tmp})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	lfs.SetSealed(true, false)

	meta := metadata.NewMemoryStore()
	const bucket = "bkt"
	if err := meta.CreateBucket(ctx, bucket); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	s := New(meta, lfs)
	hs := s.Handler()

	// PUT two objects
	type obj struct {
		key string
		b   []byte
	}
	objs := []obj{
		{"a.txt", []byte("aaa")},
		{"dir/b.txt", []byte("bbbb")},
	}
	for _, o := range objs {
		r := httptest.NewRequest(http.MethodPut, "/"+bucket+"/"+o.key, bytes.NewReader(o.b))
		w := httptest.NewRecorder()
		hs.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s expected 200, got %d: %s", o.key, w.Code, w.Body.String())
		}
	}

	// ListObjectsV2 without prefix
	r := httptest.NewRequest(http.MethodGet, "/"+bucket+"?list-type=2", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("ListObjectsV2 expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !bytes.Contains([]byte(body), []byte("<Key>a.txt</Key>")) {
		t.Fatalf("expected a.txt in list: %s", body)
	}
	if !bytes.Contains([]byte(body), []byte("<Key>dir/b.txt</Key>")) {
		t.Fatalf("expected dir/b.txt in list: %s", body)
	}
	// Basic sanity on KeyCount
	if !bytes.Contains([]byte(body), []byte("<KeyCount>2</KeyCount>")) {
		t.Fatalf("expected KeyCount 2: %s", body)
	}
	_ = time.Now() // keep time import referenced
}