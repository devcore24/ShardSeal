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
	"sort"
	"reflect"

	"s3free/pkg/metadata"
	"s3free/pkg/storage"
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
func (m *memStore) List(_ context.Context, bucket, prefix, startAfter string, maxKeys int) ([]storage.ObjectMeta, bool, error) {
	pfx := bucket + "/"
	if prefix != "" { pfx = bucket + "/" + prefix }
	var keys []string
	for k := range m.objs {
		if strings.HasPrefix(k, pfx) {
			key := strings.TrimPrefix(k, bucket+"/")
			if startAfter == "" || key > startAfter {
				keys = append(keys, key)
			}
		}
	}
	// Sort for stable results
	sort.Strings(keys)
	// Limit to maxKeys
	var result []storage.ObjectMeta
	for i, key := range keys {
		if i >= maxKeys { break }
		o := m.objs[bucket+"/"+key]
		result = append(result, storage.ObjectMeta{
			Key: key, Size: int64(len(o.data)), ETag: o.etag, LastModified: time.Now().UTC(),
		})
	}
	return result, len(keys) > maxKeys, nil
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

func TestListObjectsV2_Basic(t *testing.T) {
	meta := metadata.NewMemoryStore()
	_ = meta.CreateBucket(context.Background(), "bkt")
	objs := newMemStore()
	// Put some objects
	_, _, _ = objs.Put(context.Background(), "bkt", "a.txt", bytes.NewBufferString("a"))
	_, _, _ = objs.Put(context.Background(), "bkt", "b.txt", bytes.NewBufferString("bb"))
	_, _, _ = objs.Put(context.Background(), "bkt", "c.txt", bytes.NewBufferString("ccc"))
	s := New(meta, objs)
	hs := s.Handler()

	// List all objects
	r := httptest.NewRequest(http.MethodGet, "/bkt?list-type=2", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	body := w.Body.String()
	if !bytes.Contains(w.Body.Bytes(), []byte("a.txt")) { t.Fatalf("expected a.txt in list: %s", body) }
	if !bytes.Contains(w.Body.Bytes(), []byte("b.txt")) { t.Fatalf("expected b.txt in list: %s", body) }
	if !bytes.Contains(w.Body.Bytes(), []byte("c.txt")) { t.Fatalf("expected c.txt in list: %s", body) }
	if !bytes.Contains(w.Body.Bytes(), []byte("<KeyCount>3</KeyCount>")) { t.Fatalf("expected KeyCount 3: %s", body) }
}

func TestListObjectsV2_Prefix(t *testing.T) {
	meta := metadata.NewMemoryStore()
	_ = meta.CreateBucket(context.Background(), "bkt")
	objs := newMemStore()
	_, _, _ = objs.Put(context.Background(), "bkt", "docs/a.txt", bytes.NewBufferString("a"))
	_, _, _ = objs.Put(context.Background(), "bkt", "docs/b.txt", bytes.NewBufferString("b"))
	_, _, _ = objs.Put(context.Background(), "bkt", "images/x.png", bytes.NewBufferString("x"))
	s := New(meta, objs)
	hs := s.Handler()

	// List with prefix
	r := httptest.NewRequest(http.MethodGet, "/bkt?list-type=2&prefix=docs/", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	body := w.Body.String()
	if !bytes.Contains(w.Body.Bytes(), []byte("docs/a.txt")) { t.Fatalf("expected docs/a.txt: %s", body) }
	if !bytes.Contains(w.Body.Bytes(), []byte("docs/b.txt")) { t.Fatalf("expected docs/b.txt: %s", body) }
	if bytes.Contains(w.Body.Bytes(), []byte("images/x.png")) { t.Fatalf("should not have images/x.png: %s", body) }
	if !bytes.Contains(w.Body.Bytes(), []byte("<KeyCount>2</KeyCount>")) { t.Fatalf("expected KeyCount 2: %s", body) }
}

func TestListObjectsV2_MaxKeys(t *testing.T) {
	meta := metadata.NewMemoryStore()
	_ = meta.CreateBucket(context.Background(), "bkt")
	objs := newMemStore()
	for i := 0; i < 10; i++ {
		key := string(rune('a' + i)) + ".txt"
		_, _, _ = objs.Put(context.Background(), "bkt", key, bytes.NewBufferString("x"))
	}
	s := New(meta, objs)
	hs := s.Handler()

	// List with max-keys=3
	r := httptest.NewRequest(http.MethodGet, "/bkt?list-type=2&max-keys=3", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	body := w.Body.String()
	if !bytes.Contains(w.Body.Bytes(), []byte("<KeyCount>3</KeyCount>")) { t.Fatalf("expected KeyCount 3: %s", body) }
	if !bytes.Contains(w.Body.Bytes(), []byte("<IsTruncated>true</IsTruncated>")) { t.Fatalf("expected IsTruncated true: %s", body) }
	if !bytes.Contains(w.Body.Bytes(), []byte("<NextContinuationToken>")) { t.Fatalf("expected NextContinuationToken: %s", body) }
}

func TestListObjectsV2_Empty(t *testing.T) {
	meta := metadata.NewMemoryStore()
	_ = meta.CreateBucket(context.Background(), "bkt")
	objs := newMemStore()
	s := New(meta, objs)
	hs := s.Handler()

	r := httptest.NewRequest(http.MethodGet, "/bkt?list-type=2", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	body := w.Body.String()
	if !bytes.Contains(w.Body.Bytes(), []byte("<KeyCount>0</KeyCount>")) { t.Fatalf("expected KeyCount 0: %s", body) }
	if !bytes.Contains(w.Body.Bytes(), []byte("<IsTruncated>false</IsTruncated>")) { t.Fatalf("expected IsTruncated false: %s", body) }
}

func TestMultipartUpload_Complete(t *testing.T) {
	meta := metadata.NewMemoryStore()
	_ = meta.CreateBucket(context.Background(), "bkt")
	objs := newMemStore()
	s := New(meta, objs)
	hs := s.Handler()

	// 1. Initiate multipart upload
	r := httptest.NewRequest(http.MethodPost, "/bkt/large.bin?uploads", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("initiate expected 200, got %d", w.Code) }
	if !bytes.Contains(w.Body.Bytes(), []byte("<UploadId>")) { t.Fatalf("expected UploadId in response") }
	
	// Extract uploadId from response
	body := w.Body.String()
	start := bytes.Index(w.Body.Bytes(), []byte("<UploadId>")) + len("<UploadId>")
	end := bytes.Index(w.Body.Bytes(), []byte("</UploadId>"))
	uploadID := body[start:end]
	
	// 2. Upload part 1
	r = httptest.NewRequest(http.MethodPut, "/bkt/large.bin?uploadId="+uploadID+"&partNumber=1", bytes.NewBufferString("part1data"))
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("upload part 1 expected 200, got %d", w.Code) }
	etag1 := strings.Trim(w.Header().Get("ETag"), "\"")
	
	// 3. Upload part 2
	r = httptest.NewRequest(http.MethodPut, "/bkt/large.bin?uploadId="+uploadID+"&partNumber=2", bytes.NewBufferString("part2data"))
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("upload part 2 expected 200, got %d", w.Code) }
	etag2 := strings.Trim(w.Header().Get("ETag"), "\"")
	
	// 4. Complete multipart upload
	completeXML := `<CompleteMultipartUpload>
		<Part><PartNumber>1</PartNumber><ETag>"` + etag1 + `"</ETag></Part>
		<Part><PartNumber>2</PartNumber><ETag>"` + etag2 + `"</ETag></Part>
	</CompleteMultipartUpload>`
	r = httptest.NewRequest(http.MethodPost, "/bkt/large.bin?uploadId="+uploadID, bytes.NewBufferString(completeXML))
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("complete expected 200, got %d: %s", w.Code, w.Body.String()) }
	
	// 5. Verify final object exists
	r = httptest.NewRequest(http.MethodGet, "/bkt/large.bin", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("get final object expected 200, got %d", w.Code) }
	if got := w.Body.String(); got != "part1datapart2data" { t.Fatalf("unexpected final data: %q", got) }
}

func TestMultipartUpload_Abort(t *testing.T) {
	meta := metadata.NewMemoryStore()
	_ = meta.CreateBucket(context.Background(), "bkt")
	objs := newMemStore()
	s := New(meta, objs)
	hs := s.Handler()

	// Initiate multipart upload
	r := httptest.NewRequest(http.MethodPost, "/bkt/large.bin?uploads", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("initiate expected 200, got %d", w.Code) }
	
	body := w.Body.String()
	start := bytes.Index(w.Body.Bytes(), []byte("<UploadId>")) + len("<UploadId>")
	end := bytes.Index(w.Body.Bytes(), []byte("</UploadId>"))
	uploadID := body[start:end]
	
	// Upload a part
	r = httptest.NewRequest(http.MethodPut, "/bkt/large.bin?uploadId="+uploadID+"&partNumber=1", bytes.NewBufferString("testdata"))
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 200 { t.Fatalf("upload part expected 200, got %d", w.Code) }
	
	// Abort the upload
	r = httptest.NewRequest(http.MethodDelete, "/bkt/large.bin?uploadId="+uploadID, nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 204 { t.Fatalf("abort expected 204, got %d", w.Code) }
	
	// Verify object was not created
	r = httptest.NewRequest(http.MethodGet, "/bkt/large.bin", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != 404 { t.Fatalf("get after abort expected 404, got %d", w.Code) }
}

// helpers

func md5hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

func TestListObjectsV2_Pagination_Stability(t *testing.T) {
	ctx := context.Background()
	meta := metadata.NewMemoryStore()
	if err := meta.CreateBucket(ctx, "bkt"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	objs := newMemStore()
	// Insert 10 keys a.txt..j.txt to exercise pagination ordering and continuity
	for i := 0; i < 10; i++ {
		key := string(rune('a'+i)) + ".txt"
		_, _, _ = objs.Put(ctx, "bkt", key, bytes.NewBufferString(strings.Repeat("x", i+1)))
	}
	s := New(meta, objs)
	hs := s.Handler()

	var (
		token string
		all   []string
		page  int
	)
	for {
		page++
		url := "/bkt?list-type=2&max-keys=3"
		if token != "" {
			url += "&continuation-token=" + token
		}
		r := httptest.NewRequest(http.MethodGet, url, nil)
		w := httptest.NewRecorder()
		hs.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("page %d: expected 200, got %d: %s", page, w.Code, w.Body.String())
		}
		body := w.Body.String()
		keys := extractXMLTags(body, "Key")
		all = append(all, keys...)

		isTruncated := strings.Contains(body, "<IsTruncated>true</IsTruncated>")
		if !isTruncated {
			break
		}
		token = extractSingleXMLTag(body, "NextContinuationToken")
		if token == "" {
			t.Fatalf("page %d: missing NextContinuationToken in truncated response", page)
		}
	}

	// Expect exactly a.txt..j.txt in order
	var want []string
	for i := 0; i < 10; i++ {
		want = append(want, string(rune('a'+i))+".txt")
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("unexpected concatenated keys: got=%v want=%v", all, want)
	}
}

// extractXMLTags returns all occurrences of <tag>value</tag> in order of appearance.
func extractXMLTags(xmlStr, tag string) []string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	var out []string
	for {
		i := strings.Index(xmlStr, open)
		if i == -1 {
			break
		}
		xmlStr = xmlStr[i+len(open):]
		j := strings.Index(xmlStr, close)
		if j == -1 {
			break
		}
		out = append(out, xmlStr[:j])
		xmlStr = xmlStr[j+len(close):]
	}
	return out
}

// extractSingleXMLTag returns the first occurrence of <tag>value</tag> or "" if not found.
func extractSingleXMLTag(xmlStr, tag string) string {
	vals := extractXMLTags(xmlStr, tag)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}
