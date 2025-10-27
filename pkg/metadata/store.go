package metadata

import (
	"context"
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

// Store defines the metadata operations needed by the S3 API for buckets.
type Store interface {
	ListBuckets(ctx context.Context) ([]Bucket, error)
	CreateBucket(ctx context.Context, name string) error
	BucketExists(ctx context.Context, name string) (bool, error)
	DeleteBucket(ctx context.Context, name string) error
}

// MemoryStore is a simple in-memory implementation suitable for development
// and unit tests. It is NOT durable and should not be used in production.
type MemoryStore struct {
	mu      sync.RWMutex
	buckets map[string]Bucket
}

// NewMemoryStore creates an empty in-memory metadata store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{buckets: make(map[string]Bucket)}
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

// Errors
var (
	ErrBucketExists = errors.New("bucket already exists")
	ErrBucketNotFound = errors.New("bucket not found")
)
