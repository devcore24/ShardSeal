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
type ObjectStore interface {
	Put(ctx context.Context, bucket, key string, r io.Reader) (etag string, size int64, err error)
	Get(ctx context.Context, bucket, key string) (rc io.ReadCloser, size int64, etag string, lastModified time.Time, err error)
	Head(ctx context.Context, bucket, key string) (size int64, etag string, lastModified time.Time, err error)
	Delete(ctx context.Context, bucket, key string) error
	List(ctx context.Context, bucket, prefix, startAfter string, maxKeys int) ([]ObjectMeta, bool, error)
	IsBucketEmpty(ctx context.Context, bucket string) (bool, error)
	RemoveBucket(ctx context.Context, bucket string) error
}
