package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shardseal/pkg/erasure"
)

func md5hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

func TestLocalFS_Sealed_PutGetHeadRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	lfs, err := NewLocalFS([]string{base})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	lfs.SetSealed(true, false) // enable sealed mode, no verify-on-read for this test

	const bucket = "bkt"
	const key = "dir/obj.txt"
	payload := bytes.Repeat([]byte("abc"), 4096) // 12 KiB
	wantETag := md5hex(payload)

	etag, n, err := lfs.Put(ctx, bucket, key, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("Put size mismatch: got=%d want=%d", n, len(payload))
	}
	if etag != wantETag {
		t.Fatalf("Put etag mismatch: got=%s want=%s", etag, wantETag)
	}

	// Verify on-disk layout
	manifest := filepath.Join(lfs.base, "objects", bucket, filepath.FromSlash(key), ManifestFilename)
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("manifest not found at %s: %v", manifest, err)
	}
	shard := filepath.Join(lfs.base, "objects", bucket, filepath.FromSlash(key), "data.ss1")
	if _, err := os.Stat(shard); err != nil {
		t.Fatalf("sealed shard not found at %s: %v", shard, err)
	}

	// Head should reflect payload size and MD5 etag
	size, headETag, lastMod, err := lfs.Head(ctx, bucket, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if size != int64(len(payload)) {
		t.Fatalf("Head size mismatch: got=%d want=%d", size, len(payload))
	}
	if headETag != wantETag {
		t.Fatalf("Head etag mismatch: got=%s want=%s", headETag, wantETag)
	}
	if time.Since(lastMod) > time.Minute {
		t.Fatalf("Head last-modified looks too old: %s", lastMod)
	}

	// Get full
	rc, gsize, getETag, _, err := lfs.Get(ctx, bucket, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	if gsize != int64(len(payload)) {
		t.Fatalf("Get size mismatch: got=%d want=%d", gsize, len(payload))
	}
	if getETag != wantETag {
		t.Fatalf("Get etag mismatch: got=%s want=%s", getETag, wantETag)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read rc: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch")
	}

	// Get range via ReadSeeker
	rc2, _, _, _, err := lfs.Get(ctx, bucket, key)
	if err != nil {
		t.Fatalf("Get (2): %v", err)
	}
	defer rc2.Close()
	rs, ok := rc2.(interface{ io.ReadSeeker; io.Closer })
	if !ok {
		t.Fatalf("returned reader does not implement ReadSeeker")
	}
	const off = 100
	const count = 1024
	if _, err := rs.Seek(off, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	buf := make([]byte, count)
	nr, err := io.ReadFull(rs, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		t.Fatalf("range read: %v", err)
	}
	if !bytes.Equal(buf[:nr], payload[off:int64(off)+int64(nr)]) {
		t.Fatalf("range segment mismatch")
	}
}

func TestLocalFS_Sealed_VerifyOnRead_Corruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	lfs, err := NewLocalFS([]string{base})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	lfs.SetSealed(true, true) // enable sealed and verify-on-read

	const bucket = "bkt"
	const key = "obj.bin"
	payload := bytes.Repeat([]byte("Z"), 8192) // 8 KiB
	if _, _, err := lfs.Put(ctx, bucket, key, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Corrupt the footer contentHash byte to trigger CRC mismatch on read
	shard := filepath.Join(lfs.base, "objects", bucket, key, "data.ss1")
	f, err := os.OpenFile(shard, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open shard: %v", err)
	}
	defer f.Close()
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
	// Footer = 32 bytes contentHash + 4 bytes CRC
	foot := make([]byte, 36)
	if _, err := io.ReadFull(f, foot); err != nil {
		t.Fatalf("read footer: %v", err)
	}
	foot[0] ^= 0xFF // flip a bit in contentHash region
	if _, err := f.Seek(footerOff, io.SeekStart); err != nil {
		t.Fatalf("seek footer back: %v", err)
	}
	if _, err := f.Write(foot); err != nil {
		t.Fatalf("write footer: %v", err)
	}
	_ = f.Sync()

	// Now Get should fail integrity
	if _, _, _, _, err := lfs.Get(ctx, bucket, key); err == nil {
		t.Fatalf("expected integrity error on Get, got nil")
	} else if err != ErrIntegrity {
		t.Fatalf("expected ErrIntegrity on Get, got %v", err)
	}

	// HEAD should also fail when verifyOnRead is enabled
	if _, _, _, err := lfs.Head(ctx, bucket, key); err == nil {
		t.Fatalf("expected integrity error on Head, got nil")
	} else if err != ErrIntegrity {
		t.Fatalf("expected ErrIntegrity on Head, got %v", err)
	}
}

func TestLocalFS_Sealed_ListAndDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	lfs, err := NewLocalFS([]string{base})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	lfs.SetSealed(true, false)

	const bucket = "bkt"
	const key = "k.txt"
	data := []byte("hello sealed")
	wantSize := int64(len(data))
	wantETag := md5hex(data)

	if etag, n, err := lfs.Put(ctx, bucket, key, bytes.NewReader(data)); err != nil {
		t.Fatalf("Put: %v", err)
	} else {
		if n != wantSize || etag != wantETag {
			t.Fatalf("Put mismatch: size got=%d want=%d, etag got=%s want=%s", n, wantSize, etag, wantETag)
		}
	}

	// List should include the sealed object with size/etag from manifest
	objs, commonPrefixes, truncated, err := lfs.List(ctx, bucket, "", "", "", 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if truncated {
		t.Fatalf("unexpected truncation")
	}
	if len(commonPrefixes) > 0 { t.Fatalf("unexpected prefixes") }
	if len(objs) != 1 || objs[0].Key != key {
		t.Fatalf("unexpected list result: %+v", objs)
	}
	if objs[0].Size != wantSize || objs[0].ETag != wantETag {
		t.Fatalf("list metadata mismatch: %+v", objs[0])
	}

	// Delete should remove shard and manifest, and prune dir
	if err := lfs.Delete(ctx, bucket, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Subsequent Head should report not found
	if _, _, _, err := lfs.Head(ctx, bucket, key); err == nil {
		t.Fatalf("expected error on Head after delete")
	}
	// Files should be gone
	manifest := filepath.Join(lfs.base, "objects", bucket, key, ManifestFilename)
	if _, err := os.Stat(manifest); err == nil {
		t.Fatalf("manifest still present after delete")
	}
	shard := filepath.Join(lfs.base, "objects", bucket, key, "data.ss1")
	if _, err := os.Stat(shard); err == nil {
		t.Fatalf("sealed shard still present after delete")
	}
}
// Additional sealed-mode hardening tests

func TestLocalFS_Sealed_HeaderCRC_Corruption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	lfs, err := NewLocalFS([]string{base})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	// verifyOnRead=false is fine because header CRC is always validated by DecodeShardHeader
	lfs.SetSealed(true, false)

	const bucket = "bkt"
	const key = "hdrcrc.bin"
	payload := bytes.Repeat([]byte{0xAB}, 4096)
	if _, _, err := lfs.Put(ctx, bucket, key, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	shard := filepath.Join(lfs.base, "objects", bucket, filepath.FromSlash(key), "data.ss1")
	f, err := os.OpenFile(shard, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open shard: %v", err)
	}
	// Flip a bit in the stored CRC32C in header (last 4 bytes of 28-byte header)
	const headerSize = 28
	if _, err := f.Seek(headerSize-4, io.SeekStart); err != nil {
		_ = f.Close()
		t.Fatalf("seek header crc: %v", err)
	}
	crc := make([]byte, 4)
	if _, err := io.ReadFull(f, crc); err != nil {
		_ = f.Close()
		t.Fatalf("read header crc: %v", err)
	}
	crc[0] ^= 0xFF
	if _, err := f.Seek(headerSize-4, io.SeekStart); err != nil {
		_ = f.Close()
		t.Fatalf("seek header crc back: %v", err)
	}
	if _, err := f.Write(crc); err != nil {
		_ = f.Close()
		t.Fatalf("write header crc: %v", err)
	}
	_ = f.Close()

	// Now any sealed GET should fail due to header CRC mismatch during DecodeShardHeader
	if _, _, _, _, err := lfs.Get(ctx, bucket, key); err == nil {
		t.Fatalf("expected error on Get after header CRC corruption, got nil")
	}
}

func TestLocalFS_Sealed_TruncatedPayload_VerifyOnRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	lfs, err := NewLocalFS([]string{base})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	lfs.SetSealed(true, true) // enable verify-on-read

	const bucket = "bkt"
	const key = "trunc.bin"
	payload := bytes.Repeat([]byte("P"), 10*1024)
	if _, _, err := lfs.Put(ctx, bucket, key, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Truncate away the footer entirely (leave header+payload only)
	shard := filepath.Join(lfs.base, "objects", bucket, filepath.FromSlash(key), "data.ss1")
	f, err := os.OpenFile(shard, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open shard: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		t.Fatalf("seek: %v", err)
	}
	hdr, err := erasure.DecodeShardHeader(f)
	if err != nil {
		_ = f.Close()
		t.Fatalf("decode header: %v", err)
	}
	_ = f.Close()
	footerOff := int64(hdr.HeaderSize) + int64(hdr.PayloadLength)
	if err := os.Truncate(shard, footerOff); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Get should fail with ErrIntegrity because footer cannot be decoded
	if _, _, _, _, err := lfs.Get(ctx, bucket, key); err == nil {
		t.Fatalf("expected integrity error on Get with truncated footer, got nil")
	} else if err != ErrIntegrity {
		t.Fatalf("expected ErrIntegrity on truncated footer, got %v", err)
	}
}

func TestLocalFS_List_MixedSealedPlain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()

	lfs, err := NewLocalFS([]string{base})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}

	const bucket = "bkt"
	// Sealed object a.txt
	lfs.SetSealed(true, false)
	a := []byte("sealed-a")
	if _, _, err := lfs.Put(ctx, bucket, "a.txt", bytes.NewReader(a)); err != nil {
		t.Fatalf("Put sealed a.txt: %v", err)
	}
	// Plain object b.txt
	lfs.SetSealed(false, false)
	b := []byte("plain-b")
	if _, _, err := lfs.Put(ctx, bucket, "b.txt", bytes.NewReader(b)); err != nil {
		t.Fatalf("Put plain b.txt: %v", err)
	}

	// List: expect both keys present, ordered, with correct sizes and etags
	objs, commonPrefixes, truncated, err := lfs.List(ctx, bucket, "", "", "", 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if truncated {
		t.Fatalf("unexpected truncation")
	}
	if len(commonPrefixes) > 0 { t.Fatalf("unexpected prefixes") }
	if len(objs) != 2 {
		t.Fatalf("expected 2 objects, got %d: %+v", len(objs), objs)
	}
	if objs[0].Key != "a.txt" || objs[1].Key != "b.txt" {
		t.Fatalf("unexpected order: %+v", objs)
	}
	if objs[0].Size != int64(len(a)) || objs[1].Size != int64(len(b)) {
		t.Fatalf("size mismatch: %+v", objs)
	}
	if objs[0].ETag != md5hex(a) || objs[1].ETag != md5hex(b) {
		t.Fatalf("etag mismatch: %+v", objs)
	}
}