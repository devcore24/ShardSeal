package storage

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

 // LocalFS implements ObjectStore on a single local directory. Suitable for dev/MVP.
type LocalFS struct {
	base string // absolute base directory
	obs  storageObserver
}

 // NewLocalFS creates a LocalFS rooted at the first non-empty dir from dirs.
func NewLocalFS(dirs []string) (*LocalFS, error) {
	var base string
	for _, d := range dirs {
		if d != "" { base = d; break }
	}
	if base == "" { return nil, fmt.Errorf("no data directory configured") }
	abs, err := filepath.Abs(base)
	if err != nil { return nil, err }
	// Ensure base exists
	if err := os.MkdirAll(abs, 0o700); err != nil { return nil, err }
	return &LocalFS{base: abs}, nil
}

// storageObserver is a minimal observer used to record metrics without importing dependencies.
type storageObserver interface {
	Observe(op string, bytes int64, err error, dur time.Duration)
}

// SetObserver registers a metrics observer for storage operations.
func (l *LocalFS) SetObserver(o storageObserver) {
	l.obs = o
}

// observe emits a single observation if an observer is registered.
func (l *LocalFS) observe(op string, bytes int64, err error, start time.Time) {
	if l != nil && l.obs != nil {
		l.obs.Observe(op, bytes, err, time.Since(start))
	}
}

func (l *LocalFS) Put(ctx context.Context, bucket, key string, r io.Reader) (string, int64, error) {
	start := time.Now()
	path, err := l.objectPath(bucket, key)
	if err != nil {
		l.observe("put", 0, err, start)
		return "", 0, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		l.observe("put", 0, err, start)
		return "", 0, err
	}

	// Write to a temporary file and atomically rename to the final path.
	tmp, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		l.observe("put", 0, err, start)
		return "", 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	h := md5.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), r)
	// Attempt to flush data to disk on success before rename.
	if copyErr == nil {
		if err := tmp.Sync(); err != nil {
			copyErr = err
		}
	}
	// Close the file handle
	if cerr := tmp.Close(); copyErr == nil && cerr != nil {
		copyErr = cerr
	}
	if copyErr != nil {
		l.observe("put", n, copyErr, start)
		return "", n, copyErr
	}

	// Atomic rename to final path
	if err := os.Rename(tmpName, path); err != nil {
		l.observe("put", n, err, start)
		return "", n, err
	}

	etag := hex.EncodeToString(h.Sum(nil))
	// best-effort set mtime as last-modified
	_ = os.Chtimes(path, time.Now(), time.Now())
	l.observe("put", n, nil, start)
	return etag, n, nil
}

func (l *LocalFS) Get(ctx context.Context, bucket, key string) (io.ReadCloser, int64, string, time.Time, error) {
	start := time.Now()
	path, err := l.objectPath(bucket, key)
	if err != nil { l.observe("get", 0, err, start); return nil, 0, "", time.Time{}, err }
	st, err := os.Stat(path)
	if err != nil { l.observe("get", 0, err, start); return nil, 0, "", time.Time{}, err }
	f, err := os.Open(path)
	if err != nil { l.observe("get", 0, err, start); return nil, 0, "", time.Time{}, err }
	etag, err := md5File(path)
	if err != nil { f.Close(); l.observe("get", 0, err, start); return nil, 0, "", time.Time{}, err }
	l.observe("get", st.Size(), nil, start)
	return f, st.Size(), etag, st.ModTime().UTC(), nil
}

func (l *LocalFS) Head(ctx context.Context, bucket, key string) (int64, string, time.Time, error) {
	start := time.Now()
	path, err := l.objectPath(bucket, key)
	if err != nil { l.observe("head", 0, err, start); return 0, "", time.Time{}, err }
	st, err := os.Stat(path)
	if err != nil { l.observe("head", 0, err, start); return 0, "", time.Time{}, err }
	etag, err := md5File(path)
	if err != nil { l.observe("head", 0, err, start); return 0, "", time.Time{}, err }
	l.observe("head", 0, nil, start)
	return st.Size(), etag, st.ModTime().UTC(), nil
}

func (l *LocalFS) Delete(ctx context.Context, bucket, key string) error {
	start := time.Now()
	path, err := l.objectPath(bucket, key)
	if err != nil { l.observe("delete", 0, err, start); return err }
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) { l.observe("delete", 0, os.ErrNotExist, start); return os.ErrNotExist }
		l.observe("delete", 0, err, start)
		return err
	}
	// best-effort: remove empty parent dirs
	_ = removeEmptyParents(filepath.Dir(path), filepath.Join(l.base, "objects", bucket))
	l.observe("delete", 0, nil, start)
	return nil
}

func (l *LocalFS) List(ctx context.Context, bucket, prefix, startAfter string, maxKeys int) ([]ObjectMeta, bool, error) {
	start := time.Now()
	bdir := filepath.Join(l.base, "objects", bucket)
	var objects []ObjectMeta
 	
	err := filepath.WalkDir(bdir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) { return nil }
			return err
		}
		if d.IsDir() { return nil }
 		
		// Extract relative key from path
		rel, err := filepath.Rel(bdir, path)
		if err != nil { return err }
		key := filepath.ToSlash(rel)

		// Skip internal multipart temp files (never list these)
		if strings.HasPrefix(key, ".multipart/") { return nil }
 		
		// Apply prefix filter
		if prefix != "" && !strings.HasPrefix(key, prefix) { return nil }
 		
		// Apply startAfter filter
		if startAfter != "" && key <= startAfter { return nil }
 		
		// Get file info
		info, err := d.Info()
		if err != nil { return err }
 		
		// Compute ETag
		etag, err := md5File(path)
		if err != nil { return err }
 		
		objects = append(objects, ObjectMeta{
			Key:          key,
			Size:         info.Size(),
			ETag:         etag,
			LastModified: info.ModTime().UTC(),
		})
 		
		return nil
	})
 	
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			l.observe("list", 0, nil, start)
			return []ObjectMeta{}, false, nil
		}
		l.observe("list", 0, err, start)
		return nil, false, err
	}
 	
	// Sort by key for stable results
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})
 	
	// Apply maxKeys limit and determine if truncated
	isTruncated := len(objects) > maxKeys
	if isTruncated {
		objects = objects[:maxKeys]
	}
 	
	l.observe("list", 0, nil, start)
	return objects, isTruncated, nil
}

func (l *LocalFS) IsBucketEmpty(ctx context.Context, bucket string) (bool, error) {
	start := time.Now()
	bdir := filepath.Join(l.base, "objects", bucket)
	entries, err := os.ReadDir(bdir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) { l.observe("is_bucket_empty", 0, nil, start); return true, nil }
		l.observe("is_bucket_empty", 0, err, start)
		return false, err
	}
	// Check recursively for any files, excluding .multipart directory (internal temp files)
	var stack []string
	for _, e := range entries {
		// Skip .multipart directory - it contains internal temporary files
		if e.Name() == ".multipart" { continue }
		stack = append(stack, filepath.Join(bdir, e.Name()))
	}
	for len(stack) > 0 {
		n := len(stack)-1
		p := stack[n]
		stack = stack[:n]
		fi, err := os.Stat(p)
		if err != nil { l.observe("is_bucket_empty", 0, err, start); return false, err }
		if fi.Mode().IsRegular() {
			l.observe("is_bucket_empty", 0, nil, start)
			return false, nil
		}
		if fi.IsDir() {
			kids, err := os.ReadDir(p)
			if err != nil { l.observe("is_bucket_empty", 0, err, start); return false, err }
			for _, k := range kids { stack = append(stack, filepath.Join(p, k.Name())) }
		}
	}
	l.observe("is_bucket_empty", 0, nil, start)
	return true, nil
}

func (l *LocalFS) RemoveBucket(ctx context.Context, bucket string) error {
	bdir := filepath.Join(l.base, "objects", bucket)
	// Only remove if empty (safety); otherwise return error
	empty, err := l.IsBucketEmpty(ctx, bucket)
	if err != nil { return err }
	if !empty { return fmt.Errorf("bucket not empty") }
	// Remove the tree (will remove empty dirs)
	if err := os.RemoveAll(bdir); err != nil {
		return err
	}
	return nil
}

func removeEmptyParents(dir, stop string) error {
	for {
		if dir == stop || dir == "/" || dir == "." || dir == "" { return nil }
		e, err := os.ReadDir(dir)
		if err != nil { return nil }
		if len(e) > 0 { return nil }
		if err := os.Remove(dir); err != nil { return nil }
		next := filepath.Dir(dir)
		dir = next
	}
}

func (l *LocalFS) objectPath(bucket, key string) (string, error) {
	if bucket == "" { return "", fmt.Errorf("empty bucket") }
	cleanKey := strings.TrimPrefix(filepath.Clean("/"+key), "/")
	p := filepath.Join(l.base, "objects", bucket, cleanKey)
	abs := p
	base := l.base
	// prevent escape: ensure path starts with base
	if !strings.HasPrefix(filepath.Clean(abs)+string(os.PathSeparator), filepath.Clean(base)+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid object path")
	}
	return abs, nil
}

func md5File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil { return "", err }
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil { return "", err }
	return hex.EncodeToString(h.Sum(nil)), nil
}
