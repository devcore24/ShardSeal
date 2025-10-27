package metadata

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// Bucket represents a bucket entry in metadata storage.
type Bucket struct {
	Name         string
	CreationDate time.Time
}

// MultipartUpload represents an in-progress multipart upload.
type MultipartUpload struct {
	UploadID  string
	Bucket    string
	Key       string
	Initiated time.Time
	Parts     map[int]Part
}

// Part represents a single uploaded part in a multipart upload.
type Part struct {
	PartNumber int
	ETag       string
	Size       int64
	Uploaded   time.Time
}

// Store defines the metadata operations needed by the S3 API for buckets.
type Store interface {
	ListBuckets(ctx context.Context) ([]Bucket, error)
	CreateBucket(ctx context.Context, name string) error
	BucketExists(ctx context.Context, name string) (bool, error)
	DeleteBucket(ctx context.Context, name string) error
	
	// Multipart upload operations
	InitiateMultipartUpload(ctx context.Context, bucket, key string) (uploadID string, err error)
	GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error)
	PutPart(ctx context.Context, uploadID string, partNum int, etag string, size int64) error
	CompleteMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error)
	AbortMultipartUpload(ctx context.Context, uploadID string) error
}

// MemoryStore is a simple in-memory implementation suitable for development
// and unit tests. It is NOT durable and should not be used in production.
type MemoryStore struct {
	mu       sync.RWMutex
	buckets  map[string]Bucket
	uploads  map[string]*MultipartUpload
}

// NewMemoryStore creates an empty in-memory metadata store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		buckets: make(map[string]Bucket),
		uploads: make(map[string]*MultipartUpload),
	}
}

// ListBuckets returns all buckets sorted by name for stable output.
func (m *MemoryStore) ListBuckets(ctx context.Context) ([]Bucket, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Bucket, 0, len(m.buckets))
	for _, b := range m.buckets {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// CreateBucket creates a new bucket if it does not exist.
func (m *MemoryStore) CreateBucket(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[name]; ok {
		return ErrBucketExists
	}
	m.buckets[name] = Bucket{Name: name, CreationDate: time.Now().UTC()}
	return nil
}

// BucketExists returns true if bucket exists.
func (m *MemoryStore) BucketExists(ctx context.Context, name string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.buckets[name]
	return ok, nil
}

// DeleteBucket removes a bucket entry.
func (m *MemoryStore) DeleteBucket(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[name]; !ok {
		return ErrBucketNotFound
	}
	delete(m.buckets, name)
	return nil
}

// InitiateMultipartUpload creates a new multipart upload.
func (m *MemoryStore) InitiateMultipartUpload(ctx context.Context, bucket, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	uploadID := generateUploadID()
	m.uploads[uploadID] = &MultipartUpload{
		UploadID:  uploadID,
		Bucket:    bucket,
		Key:       key,
		Initiated: time.Now().UTC(),
		Parts:     make(map[int]Part),
	}
	return uploadID, nil
}

// GetMultipartUpload retrieves an in-progress multipart upload.
func (m *MemoryStore) GetMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	upload, ok := m.uploads[uploadID]
	if !ok {
		return nil, ErrUploadNotFound
	}
	return upload, nil
}

// PutPart adds a part to a multipart upload.
func (m *MemoryStore) PutPart(ctx context.Context, uploadID string, partNum int, etag string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	upload, ok := m.uploads[uploadID]
	if !ok {
		return ErrUploadNotFound
	}
	upload.Parts[partNum] = Part{
		PartNumber: partNum,
		ETag:       etag,
		Size:       size,
		Uploaded:   time.Now().UTC(),
	}
	return nil
}

// CompleteMultipartUpload marks an upload as complete and returns it.
func (m *MemoryStore) CompleteMultipartUpload(ctx context.Context, uploadID string) (*MultipartUpload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	upload, ok := m.uploads[uploadID]
	if !ok {
		return nil, ErrUploadNotFound
	}
	// Return a copy and remove from active uploads
	result := *upload
	delete(m.uploads, uploadID)
	return &result, nil
}

// AbortMultipartUpload cancels an in-progress upload.
func (m *MemoryStore) AbortMultipartUpload(ctx context.Context, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.uploads[uploadID]; !ok {
		return ErrUploadNotFound
	}
	delete(m.uploads, uploadID)
	return nil
}

// generateUploadID creates a unique upload ID.
func generateUploadID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Errors
var (
	ErrBucketExists   = errors.New("bucket already exists")
	ErrBucketNotFound = errors.New("bucket not found")
	ErrUploadNotFound = errors.New("multipart upload not found")
)
