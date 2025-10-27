package s3

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"time"
	"strconv"

	"s3bee/pkg/metadata"
)

// Server routes S3 requests. In early stages, it returns stubs for key APIs.
// Dependencies are injected for testability and future distribution.
type Server struct{
	store metadata.Store
	objs  objectStore
}

type objectStore interface {
	Put(ctx context.Context, bucket, key string, r io.Reader) (etag string, size int64, err error)
	Get(ctx context.Context, bucket, key string) (rc io.ReadCloser, size int64, etag string, lastModified time.Time, err error)
	Head(ctx context.Context, bucket, key string) (size int64, etag string, lastModified time.Time, err error)
	Delete(ctx context.Context, bucket, key string) error
	IsBucketEmpty(ctx context.Context, bucket string) (bool, error)
	RemoveBucket(ctx context.Context, bucket string) error
}

// New returns a new S3 API server with dependencies.
func New(store metadata.Store, objs objectStore) *Server { return &Server{store: store, objs: objs} }

// Handler returns an http.Handler for S3 routes.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.route)
}

type listBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Owner   owner    `xml:"Owner"`
	Buckets buckets  `xml:"Buckets"`
}

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type buckets struct {
	Bucket []bucket `xml:"Bucket"`
}

type bucket struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		if r.Method == http.MethodGet {
			s.handleListBuckets(w, r)
			return
		}
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Bucket path
	p := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(p, "/", 2)
	bucketName := parts[0]
	var key string
	if len(parts) == 2 { key = parts[1] }

	if key == "" {
		// Bucket-level ops
		s.handleBucket(w, r, bucketName)
		return
	}
	// Object ops
	s.handleObject(w, r, bucketName, key)
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	switch r.Method {
	case http.MethodPut:
		// Put object
		if ok, _ := s.store.BucketExists(r.Context(), bucket); !ok {
			writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", "/"+bucket, "")
			return
		}
		etag, _, err := s.objs.Put(r.Context(), bucket, key, r.Body)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", r.URL.Path, "")
			return
		}
		w.Header().Set("ETag", quoteETag(etag))
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		rc, size, etag, lastMod, err := s.objs.Get(r.Context(), bucket, key)
		if err != nil {
			writeError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", r.URL.Path, "")
			return
		}
		defer rc.Close()
		rangeHdr := r.Header.Get("Range")
		if rangeHdr == "" {
			w.Header().Set("ETag", quoteETag(etag))
			w.Header().Set("Content-Length", itoa64(size))
			w.Header().Set("Last-Modified", lastMod.Format(http.TimeFormat))
			w.WriteHeader(http.StatusOK)
			_, _ = io.Copy(w, rc)
			return
		}
		// Single-range support: bytes=start-end
		total := size
		start, end, ok := parseRange(rangeHdr, total)
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+itoa64(total))
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range cannot be satisfied.", r.URL.Path, "")
			return
		}
		// Attempt to seek for efficient range reads
		if rs, okRS := rc.(interface{ io.ReadSeeker; io.Closer }); okRS {
			_, _ = rs.Seek(start, io.SeekStart)
			remain := end - start + 1
			w.Header().Set("ETag", quoteETag(etag))
			w.Header().Set("Last-Modified", lastMod.Format(http.TimeFormat))
			w.Header().Set("Content-Range", "bytes "+itoa64(start)+"-"+itoa64(end)+"/"+itoa64(total))
			w.Header().Set("Content-Length", itoa64(remain))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.CopyN(w, rs, remain)
			return
		}
		// Fallback: read all and slice (inefficient, test-only)
		b, _ := io.ReadAll(rc)
		if int64(len(b)) != total {
			w.Header().Set("Content-Range", "bytes */"+itoa64(total))
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range cannot be satisfied.", r.URL.Path, "")
			return
		}
		w.Header().Set("ETag", quoteETag(etag))
		w.Header().Set("Last-Modified", lastMod.Format(http.TimeFormat))
		w.Header().Set("Content-Range", "bytes "+itoa64(start)+"-"+itoa64(end)+"/"+itoa64(total))
		w.Header().Set("Content-Length", itoa64(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(b[start : end+1])
	case http.MethodHead:
		size, etag, lastMod, err := s.objs.Head(r.Context(), bucket, key)
		if err != nil {
			writeError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", r.URL.Path, "")
			return
		}
		w.Header().Set("ETag", quoteETag(etag))
		w.Header().Set("Content-Length", itoa64(size))
		w.Header().Set("Last-Modified", lastMod.Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		if err := s.objs.Delete(r.Context(), bucket, key); err != nil {
			writeError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", r.URL.Path, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.", r.URL.Path, "")
	}
}

func quoteETag(s string) string { return "\"" + s + "\"" }

func itoa64(n int64) string {
	// fast int64 -> string without fmt to avoid allocations in hot path
	return strconv.FormatInt(n, 10)
}

// parseRange parses a Range header of form "bytes=start-end" (single-range only).
// Returns start, end (inclusive), and ok.
func parseRange(hdr string, total int64) (int64, int64, bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(hdr, prefix) { return 0, 0, false }
	sp := strings.TrimPrefix(hdr, prefix)
	parts := strings.SplitN(sp, ",", 2)
	seg := strings.TrimSpace(parts[0])
	se := strings.SplitN(seg, "-", 2)
	if len(se) != 2 { return 0, 0, false }
	// three cases: start-, -suffixLen, start-end
	if se[0] == "" {
		// suffix length
		suf, err := strconv.ParseInt(se[1], 10, 64)
		if err != nil || suf <= 0 { return 0, 0, false }
		if suf > total { suf = total }
		return total - suf, total - 1, true
	}
	start, err := strconv.ParseInt(se[0], 10, 64)
	if err != nil || start < 0 || start >= total { return 0, 0, false }
	if se[1] == "" {
		return start, total - 1, true
	}
	end, err := strconv.ParseInt(se[1], 10, 64)
	if err != nil || end < start || end >= total { return 0, 0, false }
	return start, end, true
}

func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bs, _ := s.store.ListBuckets(ctx)
	res := listBucketsResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner: owner{ID: "anonymous", DisplayName: "anonymous"},
	}
	for _, b := range bs {
		res.Buckets.Bucket = append(res.Buckets.Bucket, bucket{
			Name: b.Name,
			CreationDate: b.CreationDate.UTC().Format(time.RFC3339),
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(res)
}

func (s *Server) handleBucket(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodPut:
		s.handleCreateBucket(w, r, name)
	case http.MethodDelete:
		s.handleDeleteBucket(w, r, name)
	default:
		writeError(w, http.StatusNotImplemented, "NotImplemented", "Bucket operation not implemented", r.URL.Path, "")
	}
}

func (s *Server) handleDeleteBucket(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	ok, _ := s.store.BucketExists(ctx, name)
	if !ok {
		writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", "/"+name, "")
		return
	}
	empty, err := s.objs.IsBucketEmpty(ctx, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", "/"+name, "")
		return
	}
	if !empty {
		writeError(w, http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty.", "/"+name, "")
		return
	}
	if err := s.objs.RemoveBucket(ctx, name); err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", "/"+name, "")
		return
	}
	_ = s.store.DeleteBucket(ctx, name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateBucket(w http.ResponseWriter, r *http.Request, name string) {
	if !isValidBucketName(name) {
		writeError(w, http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid.", "/"+name, "")
		return
	}
	ctx := r.Context()
	ok, _ := s.store.BucketExists(ctx, name)
	if ok {
		// S3 uses 409 with BucketAlreadyOwnedByYou in many cases
		writeError(w, http.StatusConflict, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.", "/"+name, "")
		return
	}
	if err := s.store.CreateBucket(ctx, name); err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", "/"+name, "")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// S3 error response encoding (minimal)
type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
}

func writeError(w http.ResponseWriter, status int, code, message, resource, reqID string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(s3Error{Code: code, Message: message, Resource: resource, RequestID: reqID})
}

// Minimal bucket name validation per S3 guidelines (simplified):
// - 3 to 63 characters
// - lowercase letters, numbers, dots, and hyphens
// - must start and end with a letter or number
func isValidBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 { return false }
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' {
			continue
		}
		return false
	}
	first := name[0]
	last := name[len(name)-1]
	if !((first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) { return false }
	if !((last >= 'a' && last <= 'z') || (last >= '0' && last <= '9')) { return false }
	return true
}
