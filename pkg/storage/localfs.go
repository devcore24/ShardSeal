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
	"strings"
	"time"
)

// LocalFS implements ObjectStore on a single local directory. Suitable for dev/MVP.
type LocalFS struct {
	base string // absolute base directory
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

func (l *LocalFS) Put(ctx context.Context, bucket, key string, r io.Reader) (string, int64, error) {
	path, err := l.objectPath(bucket, key)
	if err != nil { return "", 0, err }
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { return "", 0, err }
	f, err := os.Create(path)
	if err != nil { return "", 0, err }
	defer f.Close()
	// compute MD5 while writing
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil { return "", n, err }
	etag := hex.EncodeToString(h.Sum(nil))
	// fs timestamp is last-modified
	_ = os.Chtimes(path, time.Now(), time.Now())
	return etag, n, nil
}

func (l *LocalFS) Get(ctx context.Context, bucket, key string) (io.ReadCloser, int64, string, time.Time, error) {
	path, err := l.objectPath(bucket, key)
	if err != nil { return nil, 0, "", time.Time{}, err }
	st, err := os.Stat(path)
	if err != nil { return nil, 0, "", time.Time{}, err }
	f, err := os.Open(path)
	if err != nil { return nil, 0, "", time.Time{}, err }
	etag, err := md5File(path)
	if err != nil { f.Close(); return nil, 0, "", time.Time{}, err }
	return f, st.Size(), etag, st.ModTime().UTC(), nil
}

func (l *LocalFS) Head(ctx context.Context, bucket, key string) (int64, string, time.Time, error) {
	path, err := l.objectPath(bucket, key)
	if err != nil { return 0, "", time.Time{}, err }
	st, err := os.Stat(path)
	if err != nil { return 0, "", time.Time{}, err }
	etag, err := md5File(path)
	if err != nil { return 0, "", time.Time{}, err }
	return st.Size(), etag, st.ModTime().UTC(), nil
}

func (l *LocalFS) Delete(ctx context.Context, bucket, key string) error {
	path, err := l.objectPath(bucket, key)
	if err != nil { return err }
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) { return os.ErrNotExist }
		return err
	}
	// best-effort: remove empty parent dirs
	_ = removeEmptyParents(filepath.Dir(path), filepath.Join(l.base, "objects", bucket))
	return nil
}

func (l *LocalFS) IsBucketEmpty(ctx context.Context, bucket string) (bool, error) {
	bdir := filepath.Join(l.base, "objects", bucket)
	entries, err := os.ReadDir(bdir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) { return true, nil }
		return false, err
	}
	// Check recursively for any files
	var stack []string
	for _, e := range entries { stack = append(stack, filepath.Join(bdir, e.Name())) }
	for len(stack) > 0 {
		n := len(stack)-1
		p := stack[n]
		stack = stack[:n]
		fi, err := os.Stat(p)
		if err != nil { return false, err }
		if fi.Mode().IsRegular() {
			return false, nil
		}
		if fi.IsDir() {
			kids, err := os.ReadDir(p)
			if err != nil { return false, err }
			for _, k := range kids { stack = append(stack, filepath.Join(p, k.Name())) }
		}
	}
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
