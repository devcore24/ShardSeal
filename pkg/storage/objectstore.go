package storage

import (
	"context"
	"io"
	"time"
)

// ObjectStore abstracts object I/O for the S3 layer.
type ObjectStore interface {
	Put(ctx context.Context, bucket, key string, r io.Reader) (etag string, size int64, err error)
	Get(ctx context.Context, bucket, key string) (rc io.ReadCloser, size int64, etag string, lastModified time.Time, err error)
	Head(ctx context.Context, bucket, key string) (size int64, etag string, lastModified time.Time, err error)
	Delete(ctx context.Context, bucket, key string) error
	IsBucketEmpty(ctx context.Context, bucket string) (bool, error)
	RemoveBucket(ctx context.Context, bucket string) error
}
