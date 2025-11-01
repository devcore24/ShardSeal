package s3

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"os"
	"context"
	"crypto/md5"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sort"
	"reflect"

	"shardseal/pkg/metadata"
	"shardseal/pkg/security/sigv4"
	"shardseal/pkg/storage"
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
	if w.Header().Get("X-S3-Error-Code") != "" { t.Fatalf("unexpected X-S3-Error-Code on success: %q", w.Header().Get("X-S3-Error-Code")) }

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
	if got := w.Header().Get("X-S3-Error-Code"); got != "NoSuchKey" {
		t.Fatalf("expected X-S3-Error-Code=NoSuchKey, got %q", got)
	}
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

func TestMultipartUpload_ETagPolicy(t *testing.T) {
	meta := metadata.NewMemoryStore()
	_ = meta.CreateBucket(context.Background(), "bkt")
	objs := newMemStore()
	s := New(meta, objs)
	hs := s.Handler()

	// 1) Initiate multipart upload
	r := httptest.NewRequest(http.MethodPost, "/bkt/large.bin?uploads", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("initiate expected 200, got %d: %s", w.Code, w.Body.String())
	}
	uploadID := extractSingleXMLTag(w.Body.String(), "UploadId")
	if uploadID == "" {
		t.Fatalf("missing UploadId in initiate response")
	}

	// 2) Upload two parts
	part1 := "part1data"
	part2 := "part2data"

	r = httptest.NewRequest(http.MethodPut, "/bkt/large.bin?uploadId="+uploadID+"&partNumber=1", bytes.NewBufferString(part1))
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("upload part 1 expected 200, got %d: %s", w.Code, w.Body.String())
	}
	etag1 := strings.Trim(w.Header().Get("ETag"), "\"")

	r = httptest.NewRequest(http.MethodPut, "/bkt/large.bin?uploadId="+uploadID+"&partNumber=2", bytes.NewBufferString(part2))
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("upload part 2 expected 200, got %d: %s", w.Code, w.Body.String())
	}
	etag2 := strings.Trim(w.Header().Get("ETag"), "\"")

	// 3) Complete upload
	completeXML := `<CompleteMultipartUpload>
		<Part><PartNumber>1</PartNumber><ETag>"` + etag1 + `"</ETag></Part>
		<Part><PartNumber>2</PartNumber><ETag>"` + etag2 + `"</ETag></Part>
	</CompleteMultipartUpload>`
	r = httptest.NewRequest(http.MethodPost, "/bkt/large.bin?uploadId="+uploadID, bytes.NewBufferString(completeXML))
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("complete expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 4) HEAD to verify ETag policy: MD5 of full final object (not AWS multipart-style ETag)
	r = httptest.NewRequest(http.MethodHead, "/bkt/large.bin", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD expected 200, got %d: %s", w.Code, w.Body.String())
	}
	want := md5hex([]byte(part1 + part2))
	if got := strings.Trim(w.Header().Get("ETag"), "\""); got != want {
		t.Fatalf("ETag policy mismatch: got=%q want=%q (MD5 of full object)", got, want)
	}
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


// --- SigV4 end-to-end tests (header-signed and presigned) ---

func TestSigV4_HeaderAuth_PutAndGet(t *testing.T) {
	ctx := context.Background()
	meta := metadata.NewMemoryStore()
	if err := meta.CreateBucket(ctx, "bkt"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	objs := newMemStore()
	s := New(meta, objs)

	// Wrap with SigV4 middleware
	ak := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	store := sigv4.NewStaticStore([]sigv4.AccessKey{{AccessKey: ak, SecretKey: secret}})
	exempt := func(r *http.Request) bool { return false }
	handler := sigv4.Middleware(store, exempt)(s.Handler())

	// 1) PUT with header auth (payload must be signed)
	const region = "us-east-1"
	const service = "s3"
	const amzDate = "20250101T120000Z"
	const date = "20250101" // scope date

	putBody := "hello-signed"
	putReq := httptest.NewRequest(http.MethodPut, "http://example.com/bkt/hello.txt", bytes.NewBufferString(putBody))
	putReq.Header.Set("X-Amz-Date", amzDate)
	// Compute SHA256 of payload and set x-amz-content-sha256
	ph := sha256HexBytes([]byte(putBody))
	putReq.Header.Set("X-Amz-Content-Sha256", strings.ToLower(ph))

	// Signed headers for PUT
	sh := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canon, err := buildCanonicalRequestForTest(putReq, sh, strings.ToLower(ph), false)
	if err != nil {
		t.Fatalf("canonical request (PUT): %v", err)
	}
	crHash := sha256HexBytes([]byte(canon))
	stringToSign := buildStringToSignForTest(amzDate, date, region, service, crHash)
	sk := deriveSigningKeyForTest(secret, date, region, service)
	signature := strings.ToLower(string(hmacSHA256HexForTest(sk, []byte(stringToSign))))
	auth := "AWS4-HMAC-SHA256 Credential=" + ak + "/" + date + "/" + region + "/" + service + "/aws4_request, " +
		"SignedHeaders=" + strings.Join(sh, ";") + ", Signature=" + signature
	putReq.Header.Set("Authorization", auth)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, putReq)
	if w.Code != 200 {
		t.Fatalf("PUT with SigV4 expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 2) GET with header auth (UNSIGNED-PAYLOAD allowed)
	getReq := httptest.NewRequest(http.MethodGet, "http://example.com/bkt/hello.txt", nil)
	getReq.Header.Set("X-Amz-Date", amzDate)
	// Signed headers for GET
	sh2 := []string{"host", "x-amz-date"}
	canon2, err := buildCanonicalRequestForTest(getReq, sh2, "UNSIGNED-PAYLOAD", false)
	if err != nil {
		t.Fatalf("canonical request (GET): %v", err)
	}
	crHash2 := sha256HexBytes([]byte(canon2))
	stringToSign2 := buildStringToSignForTest(amzDate, date, region, service, crHash2)
	signature2 := strings.ToLower(string(hmacSHA256HexForTest(sk, []byte(stringToSign2))))
	auth2 := "AWS4-HMAC-SHA256 Credential=" + ak + "/" + date + "/" + region + "/" + service + "/aws4_request, " +
		"SignedHeaders=" + strings.Join(sh2, ";") + ", Signature=" + signature2
	getReq.Header.Set("Authorization", auth2)

	w = httptest.NewRecorder()
	handler.ServeHTTP(w, getReq)
	if w.Code != 200 {
		t.Fatalf("GET with SigV4 expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != putBody {
		t.Fatalf("unexpected body: got=%q want=%q", got, putBody)
	}
}

func TestSigV4_Presigned_Get(t *testing.T) {
	ctx := context.Background()
	meta := metadata.NewMemoryStore()
	if err := meta.CreateBucket(ctx, "bkt"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	objs := newMemStore()
	_, _, _ = objs.Put(ctx, "bkt", "obj.txt", bytes.NewBufferString("data"))

	s := New(meta, objs)
	ak := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	store := sigv4.NewStaticStore([]sigv4.AccessKey{{AccessKey: ak, SecretKey: secret}})
	exempt := func(r *http.Request) bool { return false }
	handler := sigv4.Middleware(store, exempt)(s.Handler())

	const region = "us-east-1"
	const service = "s3"
	const amzDate = "20250101T120000Z"
	const date = "20250101"

	// Base request
	req := httptest.NewRequest(http.MethodGet, "http://example.com/bkt/obj.txt", nil)
	q := req.URL.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", ak+"/"+date+"/"+region+"/"+service+"/aws4_request")
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-SignedHeaders", "host")
	// No X-Amz-Content-Sha256 -> default UNSIGNED-PAYLOAD for GET in presigned path
	req.URL.RawQuery = q.Encode()

	// Build canonical request for presigned mode and sign
	canon, err := buildCanonicalRequestForTest(req, []string{"host"}, "UNSIGNED-PAYLOAD", true)
	if err != nil {
		t.Fatalf("canonical request (presigned GET): %v", err)
	}
	crHash := sha256HexBytes([]byte(canon))
	stringToSign := buildStringToSignForTest(amzDate, date, region, service, crHash)
	sk := deriveSigningKeyForTest(secret, date, region, service)
	signature := strings.ToLower(string(hmacSHA256HexForTest(sk, []byte(stringToSign))))

	q = req.URL.Query()
	q.Set("X-Amz-Signature", signature)
	req.URL.RawQuery = q.Encode()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("Presigned GET expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "data" {
		t.Fatalf("unexpected body: got=%q want=%q", got, "data")
	}
}

// --- Minimal SigV4 signing helpers for tests (duplicate of verifier logic with reduced scope) ---

func buildCanonicalRequestForTest(r *http.Request, signedHeaders []string, payloadHash string, presigned bool) (string, error) {
	method := r.Method
	uri := r.URL.EscapedPath()
	if uri == "" || uri[0] != '/' {
		uri = "/" + uri
	}
	uri = uriEncodePathForTest(uri)
	query := canonicalQueryStringForTest(r, presigned)
	canonHeaders, signed := canonicalHeadersAndListForTest(r, signedHeaders)
	if len(signed) == 0 {
		return "", fmt.Errorf("no signed headers")
	}
	signedCSV := strings.Join(signed, ";")

	var b bytes.Buffer
	b.WriteString(method)
	b.WriteByte('\n')
	b.WriteString(uri)
	b.WriteByte('\n')
	b.WriteString(query)
	b.WriteByte('\n')
	b.WriteString(canonHeaders)
	b.WriteByte('\n')
	b.WriteString(signedCSV)
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String(), nil
}

func canonicalQueryStringForTest(r *http.Request, presigned bool) string {
	values := r.URL.Query()
	keys := make([]string, 0, len(values))
	for k := range values {
		if presigned && strings.EqualFold(k, "X-Amz-Signature") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return strings.ToLower(keys[i]) < strings.ToLower(keys[j]) })
	var parts []string
	for _, k := range keys {
		vs := values[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncodeForTest(k, false)+"="+uriEncodeForTest(v, false))
		}
	}
	return strings.Join(parts, "&")
}

func canonicalHeadersAndListForTest(r *http.Request, signedHeaders []string) (string, []string) {
	need := map[string]struct{}{}
	for _, h := range signedHeaders {
		need[strings.ToLower(h)] = struct{}{}
	}
	type kv struct{ k, v string }
	var hdrs []kv
	for name := range need {
		if name == "host" {
			host := r.Host
			if host == "" {
				host = r.Header.Get("Host")
			}
			if host != "" {
				hdrs = append(hdrs, kv{k: "host", v: trimSpacesForTest(host)})
			}
			continue
		}
		val := r.Header.Get(headerCanonicalCaseForTest(name))
		if val != "" {
			hdrs = append(hdrs, kv{k: strings.ToLower(name), v: trimSpacesForTest(val)})
		}
	}
	sort.Slice(hdrs, func(i, j int) bool { return hdrs[i].k < hdrs[j].k })
	var b bytes.Buffer
	actual := make([]string, 0, len(hdrs))
	for _, h := range hdrs {
		b.WriteString(h.k)
		b.WriteByte(':')
		b.WriteString(h.v)
		b.WriteByte('\n')
		actual = append(actual, h.k)
	}
	return b.String(), actual
}

func headerCanonicalCaseForTest(s string) string {
	switch strings.ToLower(s) {
	case "content-type":
		return "Content-Type"
	case "x-amz-content-sha256":
		return "X-Amz-Content-Sha256"
	case "x-amz-date":
		return "X-Amz-Date"
	case "host":
		return "Host"
	default:
		return s
	}
}

func uriEncodeForTest(s string, encodeSlash bool) string {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else if c == '/' && !encodeSlash {
			b.WriteByte(c)
		} else {
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func uriEncodePathForTest(path string) string {
	if path == "" {
		return "/"
	}
	var b bytes.Buffer
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '/' {
			b.WriteByte('/')
			continue
		}
		if (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func sha256HexBytes(bz []byte) string {
	h := sha256.Sum256(bz)
	return hex.EncodeToString(h[:])
}

func hmacSHA256HexForTest(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	sum := m.Sum(nil)
	dst := make([]byte, hex.EncodedLen(len(sum)))
	hex.Encode(dst, sum)
	return dst
}

func deriveSigningKeyForTest(secret, date, region, service string) []byte {
	kDate := hmacSumForTest([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSumForTest(kDate, []byte(region))
	kService := hmacSumForTest(kRegion, []byte(service))
	kSigning := hmacSumForTest(kService, []byte("aws4_request"))
	return kSigning
}

func hmacSumForTest(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func buildStringToSignForTest(amzDate, date, region, service, canonicalRequestHash string) string {
	var b strings.Builder
	b.WriteString("AWS4-HMAC-SHA256\n")
	b.WriteString(amzDate)
	b.WriteByte('\n')
	b.WriteString(date)
	b.WriteByte('/')
	b.WriteString(region)
	b.WriteString("/" + service + "/aws4_request\n")
	b.WriteString(canonicalRequestHash)
	return b.String()
}

func trimSpacesForTest(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !space {
				b.WriteByte(' ')
				space = true
			}
		} else {
			space = false
			b.WriteRune(r)
		}
	}
	return b.String()
}


// --- Size limit integration tests ---

func TestSizeLimit_SinglePut_EntityTooLarge(t *testing.T) {
	ctx := context.Background()
	meta := metadata.NewMemoryStore()
	if err := meta.CreateBucket(ctx, "bkt"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	objs := newMemStore()

	// Set a very small single PUT cap to trigger EntityTooLarge
	s := NewWithLimits(meta, objs, Limits{
		SinglePutMaxBytes:    3,  // bytes
		MinMultipartPartSize: 5,  // default irrelevant here
	})
	hs := s.Handler()

	body := "1234567890" // 10 bytes
	r := httptest.NewRequest(http.MethodPut, "/bkt/big.bin", bytes.NewBufferString(body))
	r.Header.Set("Content-Length", "10")
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for EntityTooLarge, got %d: %s", w.Code, w.Body.String())
	}
	resp := w.Body.String()
	if !strings.Contains(resp, "<Code>EntityTooLarge</Code>") {
		t.Fatalf("expected S3 error code EntityTooLarge, got: %s", resp)
	}
	max := extractSingleXMLTag(resp, "MaxAllowedSize")
	if max != "3" {
		t.Fatalf("expected MaxAllowedSize=3, got %q", max)
	}
	hint := extractSingleXMLTag(resp, "Hint")
	if hint == "" {
		t.Fatalf("expected Hint element present, got: %s", resp)
	}
	if got := w.Header().Get("X-S3-Error-Code"); got != "EntityTooLarge" {
		t.Fatalf("expected X-S3-Error-Code=EntityTooLarge, got %q", got)
	}
}

func TestMultipart_MinPartSize_EntityTooSmall(t *testing.T) {
	ctx := context.Background()
	meta := metadata.NewMemoryStore()
	if err := meta.CreateBucket(ctx, "bkt"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	objs := newMemStore()

	// Configure a very small min part size (5 bytes) so our small fixture can trigger the rule.
	s := NewWithLimits(meta, objs, Limits{
		SinglePutMaxBytes:    1024, // not used here
		MinMultipartPartSize: 5,    // bytes
	})
	hs := s.Handler()

	// 1) Initiate multipart upload
	r := httptest.NewRequest(http.MethodPost, "/bkt/obj.bin?uploads", nil)
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("initiate multipart expected 200, got %d: %s", w.Code, w.Body.String())
	}
	uploadID := extractSingleXMLTag(w.Body.String(), "UploadId")
	if uploadID == "" {
		t.Fatalf("missing UploadId in initiate response")
	}

	// 2) Upload part 1 too small (1 byte)
	r = httptest.NewRequest(http.MethodPut, "/bkt/obj.bin?uploadId="+uploadID+"&partNumber=1", bytes.NewBufferString("a"))
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("upload part 1 expected 200, got %d: %s", w.Code, w.Body.String())
	}
	etag1 := strings.Trim(w.Header().Get("ETag"), "\"")

	// 3) Upload part 2 (final) >= min (5 bytes)
	r = httptest.NewRequest(http.MethodPut, "/bkt/obj.bin?uploadId="+uploadID+"&partNumber=2", bytes.NewBufferString("bcdef"))
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("upload part 2 expected 200, got %d: %s", w.Code, w.Body.String())
	}
	etag2 := strings.Trim(w.Header().Get("ETag"), "\"")

	// 4) Complete (should fail because non-final part 1 is below min when total >= min)
	completeXML := `<CompleteMultipartUpload>
		<Part><PartNumber>1</PartNumber><ETag>"` + etag1 + `"</ETag></Part>
		<Part><PartNumber>2</PartNumber><ETag>"` + etag2 + `"</ETag></Part>
	</CompleteMultipartUpload>`
	r = httptest.NewRequest(http.MethodPost, "/bkt/obj.bin?uploadId="+uploadID, bytes.NewBufferString(completeXML))
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("complete expected 400 due to EntityTooSmall, got %d: %s", w.Code, w.Body.String())
	}
	resp := w.Body.String()
	if !strings.Contains(resp, "<Code>EntityTooSmall</Code>") {
		t.Fatalf("expected S3 error code EntityTooSmall, got: %s", resp)
	}
}

func TestSinglePut_ETagPolicy_MD5(t *testing.T) {
	ctx := context.Background()
	meta := metadata.NewMemoryStore()
	if err := meta.CreateBucket(ctx, "bkt"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	objs := newMemStore()
	s := New(meta, objs)
	hs := s.Handler()

	// 1) PUT object and verify ETag equals MD5(payload)
	payload := "Hello, ETag!"
	r := httptest.NewRequest(http.MethodPut, "/bkt/obj.txt", bytes.NewBufferString(payload))
	w := httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", w.Code, w.Body.String())
	}
	want := md5hex([]byte(payload))
	if got := strings.Trim(w.Header().Get("ETag"), "\""); got != want {
		t.Fatalf("ETag mismatch on PUT: got=%q want=%q (MD5 of full object)", got, want)
	}

	// 2) HEAD and verify ETag remains MD5(payload)
	r = httptest.NewRequest(http.MethodHead, "/bkt/obj.txt", nil)
	w = httptest.NewRecorder()
	hs.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := strings.Trim(w.Header().Get("ETag"), "\""); got != want {
		t.Fatalf("ETag mismatch on HEAD: got=%q want=%q (MD5 of full object)", got, want)
	}
}
