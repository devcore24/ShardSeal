package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	erasure "shardseal/pkg/erasure"
)

var ErrIntegrity = errors.New("sealed: integrity check failed")

	// LocalFS implements ObjectStore on a single local directory. Suitable for dev/MVP.
	type LocalFS struct {
 	base         string // absolute base directory
 	obs          storageObserver
 	sealedEnabled bool  // when true, write sealed shard files + manifest
 	verifyOnRead  bool  // when true, verify footer/content hash on read (TODO in read path)
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

// SetSealed toggles sealed I/O behavior (experimental).
func (l *LocalFS) SetSealed(enabled, verifyOnRead bool) {
	l.sealedEnabled = enabled
	l.verifyOnRead = verifyOnRead
}

// observe emits a single observation if an observer is registered.
func (l *LocalFS) observe(op string, bytes int64, err error, start time.Time) {
	if l != nil && l.obs != nil {
		l.obs.Observe(op, bytes, err, time.Since(start))
	}
}

 // observeSealed records generic metrics plus sealed dimensions when supported.
func (l *LocalFS) observeSealed(op string, bytes int64, err error, start time.Time, sealed bool, integrityFail bool) {
	if l != nil && l.obs != nil {
		// base metrics
		l.obs.Observe(op, bytes, err, time.Since(start))
		// sealed-specific if available
		type sealedObserver interface {
			ObserveSealed(op string, bytes int64, err error, dur time.Duration, sealed bool, integrityFail bool)
		}
		if so, ok := l.obs.(sealedObserver); ok {
			so.ObserveSealed(op, bytes, err, time.Since(start), sealed, integrityFail)
		}
	}
}

// annotateSpan adds attributes to the current span if recording.
func annotateSpan(ctx context.Context, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span != nil && span.IsRecording() {
		span.SetAttributes(attrs...)
	}
}
// sectionReadCloser wraps an io.SectionReader with a Close that closes the underlying file.
// It implements io.ReadSeeker and io.Closer.
type sectionReadCloser struct {
	sr *io.SectionReader
	f  *os.File
}

func (s *sectionReadCloser) Read(p []byte) (int, error) { return s.sr.Read(p) }
func (s *sectionReadCloser) Seek(offset int64, whence int) (int64, error) {
	return s.sr.Seek(offset, whence)
}
func (s *sectionReadCloser) Close() error { return s.f.Close() }


func (l *LocalFS) Put(ctx context.Context, bucket, key string, r io.Reader) (string, int64, error) {
	start := time.Now()

	// Non-sealed path (default)
	if !l.sealedEnabled {
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
		// tracing: annotate non-sealed put
		annotateSpan(ctx,
			attribute.Bool("storage.sealed", false),
			attribute.String("storage.op", "put"),
		)
		l.observe("put", n, nil, start)
		return etag, n, nil
	}

	// Sealed path (ShardSeal v1): write header | payload | footer, then persist manifest.
	if bucket == "" {
		err := fmt.Errorf("empty bucket")
		l.observe("put", 0, err, start)
		return "", 0, err
	}
	cleanKey := strings.TrimPrefix(filepath.Clean("/"+key), "/")
	odir := filepath.Join(l.base, "objects", bucket, cleanKey)

	// prevent escape: ensure dir is under base
	if !strings.HasPrefix(filepath.Clean(odir)+string(os.PathSeparator), filepath.Clean(l.base)+string(os.PathSeparator)) {
		err := fmt.Errorf("invalid object path")
		l.observe("put", 0, err, start)
		return "", 0, err
	}
	if err := os.MkdirAll(odir, 0o700); err != nil {
		l.observe("put", 0, err, start)
		return "", 0, err
	}

	tmp, err := os.CreateTemp(odir, ".put-*.ss1.tmp")
	if err != nil {
		l.observe("put", 0, err, start)
		return "", 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// Reserve header space (fixed 28 bytes as per encoder) and stream payload while hashing.
	const headerReserve = 28
	if _, err := tmp.Write(make([]byte, headerReserve)); err != nil {
		_ = tmp.Close()
		l.observe("put", 0, err, start)
		return "", 0, err
	}
	sha := sha256.New()
	md := md5.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, sha, md), r)
	// Attempt to flush payload
	if copyErr == nil {
		if err := tmp.Sync(); err != nil {
			copyErr = err
		}
	}

	// Prepare footer with sha256(payload)
	var sum32 [32]byte
	copy(sum32[:], sha.Sum(nil))
	footerBytes, fErr := erasure.EncodeShardFooter(sum32)
	if copyErr == nil && fErr != nil {
		copyErr = fErr
	}
	if copyErr == nil {
		if _, err := tmp.Write(footerBytes); err != nil {
			copyErr = err
		}
	}
	if copyErr == nil {
		if err := tmp.Sync(); err != nil {
			copyErr = err
		}
	}

	// Write final header at the start
	var headerBytes []byte
	if copyErr == nil {
		hdr, herr := erasure.EncodeShardHeader(erasure.ShardHeader{
			Version:       1,           // current header version
			HeaderSize:    0,           // let encoder fill (28)
			PayloadLength: uint64(n),   // payload length
		})
		if herr != nil {
			copyErr = herr
		} else {
			headerBytes = hdr
			if _, err := tmp.Seek(0, 0); err != nil {
				copyErr = err
			} else if _, err := tmp.Write(headerBytes); err != nil {
				copyErr = err
			} else if err := tmp.Sync(); err != nil {
				copyErr = err
			}
		}
	}

	// Close temp and handle errors
	if cerr := tmp.Close(); copyErr == nil && cerr != nil {
		copyErr = cerr
	}
	if copyErr != nil {
		l.observe("put", n, copyErr, start)
		return "", n, copyErr
	}

	finalPath := filepath.Join(odir, "data.ss1")
	if err := os.Rename(tmpName, finalPath); err != nil {
		l.observe("put", n, err, start)
		return "", n, err
	}

	etag := hex.EncodeToString(md.Sum(nil))
	// Best-effort timestamp
	_ = os.Chtimes(finalPath, time.Now(), time.Now())

	// Extract CRCs from encoded header/footer
	var headerCRC32C uint32
	var footerCRC32C uint32
	if len(headerBytes) >= 4 {
		headerCRC32C = binary.LittleEndian.Uint32(headerBytes[len(headerBytes)-4:])
	}
	if len(footerBytes) >= 4 {
		footerCRC32C = binary.LittleEndian.Uint32(footerBytes[len(footerBytes)-4:])
	}

	// Persist manifest atomically
	shardRel := filepath.ToSlash(filepath.Join("objects", bucket, cleanKey, "data.ss1"))
	shard := ShardMeta{
		Path:            shardRel,
		ContentHashAlgo: "sha256",
		ContentHashHex:  hex.EncodeToString(sum32[:]),
		PayloadLength:   n,
		HeaderCRC32C:    headerCRC32C,
		FooterCRC32C:    footerCRC32C,
	}
	man := NewSingleShardManifest(bucket, key, n, etag, RSParams{K: 1, M: 0}, shard)
	if err := SaveManifest(ctx, l.base, bucket, key, man); err != nil {
		// Roll back the data file to keep invariant: no data without manifest.
		_ = os.Remove(finalPath)
		l.observe("put", n, err, start)
		return "", n, err
	}

	// tracing: annotate sealed put
	annotateSpan(ctx,
		attribute.Bool("storage.sealed", true),
		attribute.String("storage.op", "put"),
	)
	l.observeSealed("put", n, nil, start, true, false)
	return etag, n, nil
}

func (l *LocalFS) Get(ctx context.Context, bucket, key string) (io.ReadCloser, int64, string, time.Time, error) {
	start := time.Now()

	// Sealed-mode read: prefer manifest + sealed shard if present
	if m, err := LoadManifest(ctx, l.base, bucket, key); err == nil && m != nil && len(m.Shards) > 0 {
		shard := m.Shards[0]
		sp := filepath.Join(l.base, filepath.FromSlash(shard.Path))
		f, err := os.Open(sp)
		if err != nil {
			l.observe("get", 0, err, start)
			return nil, 0, "", time.Time{}, err
		}
		// Decode header to obtain payload offset/length
		if _, err := f.Seek(0, 0); err != nil {
			_ = f.Close()
			l.observe("get", 0, err, start)
			return nil, 0, "", time.Time{}, err
		}
		hdr, err := erasure.DecodeShardHeader(f)
		if err != nil {
			_ = f.Close()
			l.observe("get", 0, err, start)
			return nil, 0, "", time.Time{}, err
		}

		// Optional integrity verification on read
		if l.verifyOnRead {
			// Read footer and verify CRC + manifest hash
			footerOff := int64(hdr.HeaderSize) + int64(hdr.PayloadLength)
			if _, err := f.Seek(footerOff, io.SeekStart); err != nil {
				_ = f.Close()
				l.observe("get", 0, err, start)
				return nil, 0, "", time.Time{}, err
			}
			footer, err := erasure.DecodeShardFooter(f)
			if err != nil {
				_ = f.Close()
				// tracing: integrity failure
				annotateSpan(ctx,
					attribute.Bool("storage.sealed", true),
					attribute.Bool("storage.integrity_fail", true),
					attribute.String("storage.op", "get"),
				)
				l.observeSealed("get", 0, ErrIntegrity, start, true, true)
				return nil, 0, "", time.Time{}, ErrIntegrity
			}
			// Compare footer content hash with manifest
			wantBytes, derr := hex.DecodeString(shard.ContentHashHex)
			if derr != nil || len(wantBytes) != len(footer.ContentHash) {
				_ = f.Close()
				annotateSpan(ctx,
					attribute.Bool("storage.sealed", true),
					attribute.Bool("storage.integrity_fail", true),
					attribute.String("storage.op", "get"),
				)
				l.observeSealed("get", 0, ErrIntegrity, start, true, true)
				return nil, 0, "", time.Time{}, ErrIntegrity
			}
			if !bytes.Equal(wantBytes, footer.ContentHash[:]) {
				_ = f.Close()
				annotateSpan(ctx,
					attribute.Bool("storage.sealed", true),
					attribute.Bool("storage.integrity_fail", true),
					attribute.String("storage.op", "get"),
				)
				l.observeSealed("get", 0, ErrIntegrity, start, true, true)
				return nil, 0, "", time.Time{}, ErrIntegrity
			}
			// Recompute payload sha256 to verify matches footer
			if _, err := f.Seek(int64(hdr.HeaderSize), io.SeekStart); err != nil {
				_ = f.Close()
				l.observe("get", 0, err, start)
				return nil, 0, "", time.Time{}, err
			}
			h := sha256.New()
			if _, err := io.CopyN(h, f, int64(hdr.PayloadLength)); err != nil {
				_ = f.Close()
				annotateSpan(ctx,
					attribute.Bool("storage.sealed", true),
					attribute.Bool("storage.integrity_fail", true),
					attribute.String("storage.op", "get"),
				)
				l.observeSealed("get", 0, ErrIntegrity, start, true, true)
				return nil, 0, "", time.Time{}, ErrIntegrity
			}
			var sum [32]byte
			copy(sum[:], h.Sum(nil))
			if !bytes.Equal(sum[:], footer.ContentHash[:]) {
				_ = f.Close()
				annotateSpan(ctx,
					attribute.Bool("storage.sealed", true),
					attribute.Bool("storage.integrity_fail", true),
					attribute.String("storage.op", "get"),
				)
				l.observeSealed("get", 0, ErrIntegrity, start, true, true)
				return nil, 0, "", time.Time{}, ErrIntegrity
			}
		}

		// Return a reader over the payload section
		size := int64(hdr.PayloadLength)
		rc := &sectionReadCloser{sr: io.NewSectionReader(f, int64(hdr.HeaderSize), size), f: f}
		// tracing: annotate sealed get success
		annotateSpan(ctx,
			attribute.Bool("storage.sealed", true),
			attribute.String("storage.op", "get"),
		)
		l.observeSealed("get", size, nil, start, true, false)
		return rc, size, m.ETag, m.LastModified, nil
	}

	// Fallback: plain file object
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

	// Sealed object: read from manifest if present
	if m, err := LoadManifest(ctx, l.base, bucket, key); err == nil && m != nil && len(m.Shards) > 0 {
		// Optional light verification on HEAD: check footer CRC and hash matches manifest
		if l.verifyOnRead {
			shard := m.Shards[0]
			sp := filepath.Join(l.base, filepath.FromSlash(shard.Path))
			f, err := os.Open(sp)
			if err != nil {
				l.observe("head", 0, err, start)
				return 0, "", time.Time{}, err
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				_ = f.Close()
				l.observe("head", 0, err, start)
				return 0, "", time.Time{}, err
			}
			hdr, err := erasure.DecodeShardHeader(f)
			if err != nil {
				_ = f.Close()
				l.observeSealed("head", 0, ErrIntegrity, start, true, true)
				return 0, "", time.Time{}, ErrIntegrity
			}
			footerOff := int64(hdr.HeaderSize) + int64(hdr.PayloadLength)
			if _, err := f.Seek(footerOff, io.SeekStart); err != nil {
				_ = f.Close()
				l.observe("head", 0, err, start)
				return 0, "", time.Time{}, err
			}
			footer, err := erasure.DecodeShardFooter(f)
			if err != nil {
				_ = f.Close()
				l.observeSealed("head", 0, ErrIntegrity, start, true, true)
				return 0, "", time.Time{}, ErrIntegrity
			}
			_ = f.Close()
			// Compare footer content hash with manifest
			wantBytes, derr := hex.DecodeString(shard.ContentHashHex)
			if derr != nil || !bytes.Equal(wantBytes, footer.ContentHash[:]) {
				l.observeSealed("head", 0, ErrIntegrity, start, true, true)
				return 0, "", time.Time{}, ErrIntegrity
			}
		}
		l.observe("head", 0, nil, start)
		return m.Size, m.ETag, m.LastModified, nil
	}

	// Fallback: plain file
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

	// Try sealed object deletion first (manifest-driven)
	if m, err := LoadManifest(ctx, l.base, bucket, key); err == nil && m != nil && len(m.Shards) > 0 {
		shard := m.Shards[0]
		sp := filepath.Join(l.base, filepath.FromSlash(shard.Path))
		_ = os.Remove(sp) // ignore error; proceed to remove manifest
		if mp, err := manifestPath(l.base, bucket, key); err == nil {
			_ = os.Remove(mp)
		}
		cleanKey := strings.TrimPrefix(filepath.Clean("/"+key), "/")
		// Best-effort: remove object directory if empty
		_ = removeEmptyParents(filepath.Join(l.base, "objects", bucket, cleanKey), filepath.Join(l.base, "objects", bucket))
		l.observe("delete", 0, nil, start)
		return nil
	}

	// Fallback: plain file deletion
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
  		
		// Extract relative path from bucket dir
		rel, err := filepath.Rel(bdir, path)
		if err != nil { return err }
		relSlash := filepath.ToSlash(rel)

		// Skip internal multipart temp files (never list these)
		if strings.HasPrefix(relSlash, ".multipart/") { return nil }

		name := d.Name()
		// Skip manifest files themselves
		if name == ManifestFilename { return nil }

		// Sealed object file: data.ss1 maps to object key = parent directory
		if name == "data.ss1" {
			objKey := filepath.ToSlash(filepath.Dir(rel))
			// Apply filters
			if prefix != "" && !strings.HasPrefix(objKey, prefix) { return nil }
			if startAfter != "" && objKey <= startAfter { return nil }
			// Load manifest for metadata (ETag, size, last modified)
			m, merr := LoadManifest(ctx, l.base, bucket, objKey)
			if merr != nil || m == nil {
				// If manifest missing, skip inconsistent sealed object
				return nil
			}
			objects = append(objects, ObjectMeta{
				Key:          objKey,
				Size:         m.Size,
				ETag:         m.ETag,
				LastModified: m.LastModified,
			})
			return nil
		}

		// Plain file object
		key := relSlash
		// Apply filters
		if prefix != "" && !strings.HasPrefix(key, prefix) { return nil }
		if startAfter != "" && key <= startAfter { return nil }
		info, err := d.Info()
		if err != nil { return err }
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
