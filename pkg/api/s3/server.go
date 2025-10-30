package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"strconv"

	"s3free/pkg/metadata"
	"s3free/pkg/storage"
)

// S3 API constants
const (
s3Xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"

// Error codes
errCodeNoSuchBucket        = "NoSuchBucket"
errCodeNoSuchKey           = "NoSuchKey"
errCodeBucketNotEmpty      = "BucketNotEmpty"
errCodeBucketAlreadyExists = "BucketAlreadyOwnedByYou"
errCodeInvalidBucketName   = "InvalidBucketName"
errCodeInternalError       = "InternalError"
errCodeMethodNotAllowed    = "MethodNotAllowed"
errCodeNotImplemented      = "NotImplemented"
errCodeInvalidRange        = "InvalidRange"
errCodeInvalidArgument     = "InvalidArgument"
errCodeNoSuchUpload        = "NoSuchUpload"
errCodeInvalidPart         = "InvalidPart"
errCodeMalformedXML        = "MalformedXML"
errCodeRangeNotSatisfiable = "RequestedRangeNotSatisfiable"
errCodeEntityTooLarge      = "EntityTooLarge"
errCodeEntityTooSmall      = "EntityTooSmall"

// Query parameters
queryListType          = "list-type"
queryPrefix            = "prefix"
queryContinuationToken = "continuation-token"
queryStartAfter        = "start-after"
queryMaxKeys           = "max-keys"
queryUploadID          = "uploadId"
queryPartNumber        = "partNumber"
queryUploads           = "uploads"

// Internal staging bucket for multipart temp parts (outside user buckets)
internalMultipartBucket = ".multipart"

// Limits (align with S3 semantics)
singlePutMaxBytes     = 5 * 1024 * 1024 * 1024 // 5 GiB single PUT cap
minMultipartPartSize  = 5 * 1024 * 1024        // 5 MiB for all parts except last
)

// capReader utilities for enforcing streaming size caps on single PUTs.
var errTooLarge = errors.New("entity too large")

// capReader enforces a maximum number of bytes read; returns errTooLarge when exceeded.
// It never buffers beyond the capped amount and supports underlying streaming readers.
type capReader struct {
r   io.Reader
max int64
n   int64
}

func (c *capReader) Read(p []byte) (int, error) {
if c.n >= c.max {
	return 0, errTooLarge
}
remain := c.max - c.n
if int64(len(p)) > remain {
	p = p[:remain]
}
n, err := c.r.Read(p)
c.n += int64(n)
if err != nil {
	return n, err
}
if c.n >= c.max {
	return n, errTooLarge
}
return n, nil
}

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
	List(ctx context.Context, bucket, prefix, startAfter string, maxKeys int) ([]storage.ObjectMeta, bool, error)
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
	q := r.URL.Query()
	
	// Check for multipart upload operations
	if uploadID := q.Get("uploadId"); uploadID != "" {
		if r.Method == http.MethodPost {
			s.handleCompleteMultipartUpload(w, r, bucket, key, uploadID)
			return
		} else if r.Method == http.MethodDelete {
			s.handleAbortMultipartUpload(w, r, bucket, key, uploadID)
			return
		} else if r.Method == http.MethodPut && q.Get("partNumber") != "" {
			s.handleUploadPart(w, r, bucket, key, uploadID)
			return
		}
	}
	
	// Check for multipart upload initiation
	if _, hasUploads := q["uploads"]; hasUploads && r.Method == http.MethodPost {
		s.handleInitiateMultipartUpload(w, r, bucket, key)
		return
	}
	
	switch r.Method {
	case http.MethodPut:
		// Put object (single-part). Enforce S3-like single PUT size cap (5 GiB).
		if ok, _ := s.store.BucketExists(r.Context(), bucket); !ok {
			writeError(w, http.StatusNotFound, errCodeNoSuchBucket, "The specified bucket does not exist.", "/"+bucket, "")
			return
		}
		// If Content-Length is present and exceeds cap, reject early.
		if cl := r.Header.Get("Content-Length"); cl != "" {
			if v, err := strconv.ParseInt(cl, 10, 64); err == nil && v > singlePutMaxBytes {
				writeError(w, http.StatusBadRequest, errCodeEntityTooLarge, "Object size exceeds single PUT limit; use Multipart Upload.", r.URL.Path, "")
				return
			}
		}
		reader := &capReader{r: r.Body, max: singlePutMaxBytes}
		etag, _, err := s.objs.Put(r.Context(), bucket, key, reader)
		if err != nil {
			if errors.Is(err, errTooLarge) {
				writeError(w, http.StatusBadRequest, errCodeEntityTooLarge, "Object size exceeds single PUT limit; use Multipart Upload.", r.URL.Path, "")
				return
			}
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "We encountered an internal error. Please try again.", r.URL.Path, "")
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
		// Range requests require seekable storage - return error if not supported
		writeError(w, http.StatusNotImplemented, "NotImplemented", "Range requests are not supported for this storage backend.", r.URL.Path, "")
		return
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
		Xmlns: s3Xmlns,
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
	case http.MethodGet:
		// Check for list-type=2 query param for ListObjectsV2
		if r.URL.Query().Get("list-type") == "2" {
			s.handleListObjectsV2(w, r, name)
			return
		}
		writeError(w, http.StatusNotImplemented, "NotImplemented", "ListObjects (v1) not implemented; use list-type=2", r.URL.Path, "")
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
		writeError(w, http.StatusNotFound, errCodeNoSuchBucket, "The specified bucket does not exist.", "/"+name, "")
		return
	}
	// Guard: block deletion when active multipart uploads exist for this bucket
	if n, err := s.store.ActiveMultipartCount(ctx, name); err == nil && n > 0 {
		writeError(w, http.StatusConflict, errCodeBucketNotEmpty, "The bucket you tried to delete has active multipart uploads.", "/"+name, "")
		return
	}

	empty, err := s.objs.IsBucketEmpty(ctx, name)
	if err != nil {
		slog.Error("failed to check if bucket is empty", "bucket", name, "error", err)
		writeError(w, http.StatusInternalServerError, errCodeInternalError, "We encountered an internal error. Please try again.", "/"+name, "")
		return
	}
	if !empty {
		writeError(w, http.StatusConflict, errCodeBucketNotEmpty, "The bucket you tried to delete is not empty.", "/"+name, "")
		return
	}
	if err := s.objs.RemoveBucket(ctx, name); err != nil {
		slog.Error("failed to remove bucket from object store", "bucket", name, "error", err)
		writeError(w, http.StatusInternalServerError, errCodeInternalError, "We encountered an internal error. Please try again.", "/"+name, "")
		return
	}
	// Delete bucket from metadata store - log error but don't fail the request
	// since the physical bucket is already removed
	if err := s.store.DeleteBucket(ctx, name); err != nil {
		slog.Error("failed to delete bucket from metadata store (bucket already removed from storage)", "bucket", name, "error", err)
	}
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

// ListObjectsV2 response structures
type listObjectsV2Result struct {
	XMLName               xml.Name        `xml:"ListBucketResult"`
	Xmlns                 string          `xml:"xmlns,attr"`
	Name                  string          `xml:"Name"`
	Prefix                string          `xml:"Prefix"`
	MaxKeys               int             `xml:"MaxKeys"`
	KeyCount              int             `xml:"KeyCount"`
	IsTruncated           bool            `xml:"IsTruncated"`
	ContinuationToken     string          `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string          `xml:"NextContinuationToken,omitempty"`
	StartAfter            string          `xml:"StartAfter,omitempty"`
	Contents              []objectContent `xml:"Contents"`
}

type objectContent struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

func (s *Server) handleListObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) {
	ctx := r.Context()
	ok, _ := s.store.BucketExists(ctx, bucket)
	if !ok {
		writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", "/"+bucket, "")
		return
	}

	q := r.URL.Query()
	prefix := q.Get("prefix")
	continuationToken := q.Get("continuation-token")
	startAfter := q.Get("start-after")
	maxKeysStr := q.Get("max-keys")

	// Default max-keys is 1000 per S3 spec
	maxKeys := 1000
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 {
			maxKeys = mk
		}
	}

	// continuation-token takes precedence over start-after
	if continuationToken != "" {
		startAfter = continuationToken
	}

	// Request one extra to determine if truncated
	objects, isTruncated, err := s.objs.List(ctx, bucket, prefix, startAfter, maxKeys+1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", r.URL.Path, "")
		return
	}

	// If we got more than maxKeys, we're truncated
	var nextToken string
	if len(objects) > maxKeys {
		isTruncated = true
		nextToken = objects[maxKeys-1].Key
		objects = objects[:maxKeys]
	}

	result := listObjectsV2Result{
		Xmlns:         s3Xmlns,
		Name:          bucket,
		Prefix:        prefix,
		MaxKeys:       maxKeys,
		KeyCount:      len(objects),
		IsTruncated:   isTruncated,
		StartAfter:    startAfter,
	}

	if continuationToken != "" {
		result.ContinuationToken = continuationToken
	}
	if isTruncated && nextToken != "" {
		result.NextContinuationToken = nextToken
	}

	for _, obj := range objects {
		result.Contents = append(result.Contents, objectContent{
			Key:          obj.Key,
			LastModified: obj.LastModified.UTC().Format(time.RFC3339),
			ETag:         quoteETag(obj.ETag),
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}

// Multipart upload handlers

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

func (s *Server) handleInitiateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	ctx := r.Context()
	ok, _ := s.store.BucketExists(ctx, bucket)
	if !ok {
		writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", "/"+bucket, "")
		return
	}
	
	uploadID, err := s.store.InitiateMultipartUpload(ctx, bucket, key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", r.URL.Path, "")
		return
	}
	
	result := initiateMultipartUploadResult{
		Xmlns:    s3Xmlns,
		Bucket:   bucket,
		Key:      key,
		UploadID: uploadID,
	}
	
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}

func (s *Server) handleUploadPart(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	ctx := r.Context()
	partNumStr := r.URL.Query().Get("partNumber")
	partNum, err := strconv.Atoi(partNumStr)
	if err != nil || partNum < 1 || partNum > 10000 {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "Part number must be an integer between 1 and 10000.", r.URL.Path, "")
		return
	}

	// Verify upload exists
	_, err = s.store.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		writeError(w, http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.", r.URL.Path, "")
		return
	}

	// Upload the part to internal staging bucket outside user namespace
	// Key layout: "<bucket>/<object-key>/<uploadID>/part.<N>"
	partKey := bucket + "/" + key + "/" + uploadID + "/part." + strconv.Itoa(partNum)
	etag, size, err := s.objs.Put(ctx, internalMultipartBucket, partKey, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", r.URL.Path, "")
		return
	}

	// Record the part in metadata
	err = s.store.PutPart(ctx, uploadID, partNum, etag, size)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", r.URL.Path, "")
		return
	}

	w.Header().Set("ETag", quoteETag(etag))
	w.WriteHeader(http.StatusOK)
}

type completeMultipartUpload struct {
	XMLName xml.Name               `xml:"CompleteMultipartUpload"`
	Parts   []completedPartRequest `xml:"Part"`
}

type completedPartRequest struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func (s *Server) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	ctx := r.Context()
	
	// Parse the complete request body
	var req completeMultipartUpload
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed.", r.URL.Path, "")
		return
	}
	
	// Get the upload metadata
	upload, err := s.store.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		writeError(w, http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.", r.URL.Path, "")
		return
	}
	
		// Verify all parts are present and match
		for _, reqPart := range req.Parts {
			part, ok := upload.Parts[reqPart.PartNumber]
			if !ok {
				writeError(w, http.StatusBadRequest, errCodeInvalidPart, "One or more of the specified parts could not be found.", r.URL.Path, "")
				return
			}
			// ETags in request may be quoted, so compare without quotes
			reqETag := strings.Trim(reqPart.ETag, "\"")
			if part.ETag != reqETag {
				writeError(w, http.StatusBadRequest, errCodeInvalidPart, "One or more of the specified parts has an incorrect ETag.", r.URL.Path, "")
				return
			}
		}
		// Enforce minimum part size (5 MiB) for all parts except the last listed part.
		if len(req.Parts) > 1 {
			// Only enforce minimum part size when final object size is at least the threshold.
			var totalSize int64
			for _, p := range req.Parts {
				if part, ok := upload.Parts[p.PartNumber]; ok {
					totalSize += part.Size
				}
			}
			if totalSize >= minMultipartPartSize {
				lastNum := 0
				for _, p := range req.Parts {
					if p.PartNumber > lastNum {
						lastNum = p.PartNumber
					}
				}
				for _, p := range req.Parts {
					if p.PartNumber == lastNum {
						continue // last part may be smaller
					}
					part := upload.Parts[p.PartNumber]
					if part.Size < minMultipartPartSize {
						writeError(w, http.StatusBadRequest, errCodeEntityTooSmall, "Each part except the last must be at least 5 MiB.", r.URL.Path, "")
						return
					}
				}
			}
		}
		
		// Combine parts into final object using streaming to avoid loading all parts into memory
	var partReaders []io.Reader
	var partClosers []io.ReadCloser

	for i := 1; i <= len(upload.Parts); i++ {
		partKey := bucket + "/" + key + "/" + uploadID + "/part." + strconv.Itoa(i)
		rc, _, _, _, err := s.objs.Get(ctx, internalMultipartBucket, partKey)
		if err != nil {
			// Close any already opened readers
			for _, closer := range partClosers {
				closer.Close()
			}
			writeError(w, http.StatusInternalServerError, "InternalError", "Failed to retrieve part data.", r.URL.Path, "")
			return
		}
		partReaders = append(partReaders, rc)
		partClosers = append(partClosers, rc)
	}

	// Ensure all part readers are closed after use
	defer func() {
		for _, closer := range partClosers {
			closer.Close()
		}
	}()

	// Create a multi-reader that streams from all parts in sequence
	combinedReader := io.MultiReader(partReaders...)

	// Write the final object by streaming from the combined reader
	etag, _, err := s.objs.Put(ctx, bucket, key, combinedReader)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "Failed to create final object.", r.URL.Path, "")
		return
	}

	// Clean up parts
	for i := 1; i <= len(upload.Parts); i++ {
		partKey := bucket + "/" + key + "/" + uploadID + "/part." + strconv.Itoa(i)
		_ = s.objs.Delete(ctx, internalMultipartBucket, partKey)
	}
	
	// Complete the upload in metadata
	_, err = s.store.CompleteMultipartUpload(ctx, uploadID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", r.URL.Path, "")
		return
	}
	
	result := completeMultipartUploadResult{
		Xmlns:    s3Xmlns,
		Location: "/" + bucket + "/" + key,
		Bucket:   bucket,
		Key:      key,
		ETag:     quoteETag(etag),
	}
	
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}

func (s *Server) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	ctx := r.Context()
	
	// Get upload metadata to clean up parts
	upload, err := s.store.GetMultipartUpload(ctx, uploadID)
	if err != nil {
		writeError(w, http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.", r.URL.Path, "")
		return
	}
	
	// Delete all parts
	for partNum := range upload.Parts {
		partKey := bucket + "/" + key + "/" + uploadID + "/part." + strconv.Itoa(partNum)
		_ = s.objs.Delete(ctx, internalMultipartBucket, partKey)
	}
	
	// Abort in metadata
	err = s.store.AbortMultipartUpload(ctx, uploadID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", r.URL.Path, "")
		return
	}
	
	w.WriteHeader(http.StatusNoContent)
}
