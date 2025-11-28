package storage

import (
	"context"
	"io"
	"time"
)

// ObjectMeta holds metadata for a listed object.
type ObjectMeta struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// ObjectStore abstracts object I/O for the S3 layer.
//
// Concurrency Safety: All implementations of ObjectStore MUST be safe for concurrent
// use by multiple goroutines. All methods must handle concurrent reads and writes
// correctly. For operations that modify state (Put, Delete, RemoveBucket), implementations
// should use appropriate locking or atomic operations to ensure consistency.
//
// Performance Note: Get() should return an io.ReadCloser that implements io.ReadSeeker
// when possible to support efficient range requests. If the returned reader does not
// implement io.ReadSeeker, range requests will fail with a NotImplemented error.
type ObjectStore interface {
	Put(ctx context.Context, bucket, key string, r io.Reader) (etag string, size int64, err error)
	Get(ctx context.Context, bucket, key string) (rc io.ReadCloser, size int64, etag string, lastModified time.Time, err error)
	Head(ctx context.Context, bucket, key string) (size int64, etag string, lastModified time.Time, err error)
	Delete(ctx context.Context, bucket, key string) error
	List(ctx context.Context, bucket, prefix, startAfter, delimiter string, maxKeys int) ([]ObjectMeta, []string, bool, error)
	IsBucketEmpty(ctx context.Context, bucket string) (bool, error)
	RemoveBucket(ctx context.Context, bucket string) error
}
