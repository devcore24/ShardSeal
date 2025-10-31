package repair

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	erasure "shardseal/pkg/erasure"
	"shardseal/pkg/storage"
)


// TestSealedScrubber_RunOnce_OKAndDetectCorruption verifies that the SealedScrubber
// passes a healthy sealed object and later detects a footer tampering corruption.
func TestSealedScrubber_RunOnce_OKAndDetectCorruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Set up a LocalFS with sealed mode enabled to create a sealed object.
	base := t.TempDir()
	lfs, err := storage.NewLocalFS([]string{base})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	lfs.SetSealed(true, false)

	const bucket = "bkt"
	const key = "dir/obj.txt"
	payload := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")

	// Write sealed object (header | payload | footer + manifest).
	etag, n, err := lfs.Put(ctx, bucket, key, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("LocalFS.Put: %v", err)
	}
	if etag == "" || n != int64(len(payload)) {
		t.Fatalf("unexpected put result etag=%q n=%d", etag, n)
	}

	// Create scrubber with payload verification on.
	ss := NewSealedScrubber([]string{base}, Config{
		Interval:      time.Hour,
		Concurrency:   1,
		VerifyPayload: true,
	})

	// Run once with healthy object -> expect scanned=1, errors=0.
	if err := ss.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (healthy): %v", err)
	}
	s := ss.Stats()
	if s.Scanned == 0 {
		t.Fatalf("expected Scanned > 0, got %d", s.Scanned)
	}
	if s.Errors != 0 {
		t.Fatalf("expected Errors=0 on healthy object, got %d", s.Errors)
	}

	// Tamper the sealed shard footer to trigger detection.
	shard := filepath.Join(base, "objects", bucket, filepath.FromSlash(key), "data.ss1")
	f, err := os.OpenFile(shard, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open shard: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	hdr, err := erasure.DecodeShardHeader(f)
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	footerOff := int64(hdr.HeaderSize) + int64(hdr.PayloadLength)
	if _, err := f.Seek(footerOff, io.SeekStart); err != nil {
		t.Fatalf("seek footer: %v", err)
	}
	foot := make([]byte, 36) // 32 sha256 + 4 crc32c
	if _, err := io.ReadFull(f, foot); err != nil {
		t.Fatalf("read footer: %v", err)
	}
	// Flip a bit in content hash to invalidate footer/content hash verification.
	foot[0] ^= 0xFF
	if _, err := f.Seek(footerOff, io.SeekStart); err != nil {
		t.Fatalf("seek back: %v", err)
	}
	if _, err := f.Write(foot); err != nil {
		t.Fatalf("write footer: %v", err)
	}
	_ = f.Close()

	// Run scrub again -> expect errors > 0.
	if err := ss.RunOnce(ctx); err == nil {
		// verifyObject returns error when any shard fails; doRunOnce returns last error (optional).
		// We don't strictly require a non-nil error here; Stats is authoritative.
	}

	s2 := ss.Stats()
	if s2.Errors == 0 {
		t.Fatalf("expected Errors > 0 after corruption, got %d", s2.Errors)
	}
	if s2.Scanned < 2 {
		t.Fatalf("expected Scanned >= 2 after two runs, got %d", s2.Scanned)
	}
}