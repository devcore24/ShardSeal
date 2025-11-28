package repair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	erasure "shardseal/pkg/erasure"
	"shardseal/pkg/storage"
)

func TestSingleShardRewriterRepairsHeader(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	lfs, err := storage.NewLocalFS([]string{base})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	lfs.SetSealed(true, true)

	_, _, err = lfs.Put(ctx, "bkt", "obj.txt", bytes.NewReader([]byte("hello repair")))
	if err != nil {
		t.Fatalf("Put sealed object: %v", err)
	}
	origManifest, err := storage.LoadManifest(ctx, base, "bkt", "obj.txt")
	if err != nil {
		t.Fatalf("load original manifest: %v", err)
	}
	origHash := origManifest.Shards[0].ContentHashHex
	if origManifest.Shards[0].PayloadLength != int64(len("hello repair")) {
		t.Fatalf("unexpected payload length: got %d", origManifest.Shards[0].PayloadLength)
	}

	// Corrupt the shard header to simulate an integrity failure.
	shardPath := filepath.Join(base, "objects", "bkt", "obj.txt", "data.ss1")
	f, err := os.OpenFile(shardPath, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open shard: %v", err)
	}
	if _, err := f.WriteAt([]byte("corruptheaderbytes!!!!"), 0); err != nil {
		t.Fatalf("corrupt header: %v", err)
	}
	_ = f.Close()

	rewriter := NewSingleShardRewriter(base)
	item := RepairItem{
		Bucket:    "bkt",
		Key:       "obj.txt",
		ShardPath: filepath.ToSlash(filepath.Join("objects", "bkt", "obj.txt", "data.ss1")),
		Reason:    "read_integrity_fail",
	}
	if err := rewriter.Repair(ctx, item); err != nil {
		t.Fatalf("Repair: %v", err)
	}

	m, err := storage.LoadManifest(ctx, base, "bkt", "obj.txt")
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	sp := filepath.Join(base, filepath.FromSlash(m.Shards[0].Path))
	shardFile, err := os.Open(sp)
	if err != nil {
		t.Fatalf("open shard: %v", err)
	}
	defer shardFile.Close()
	hdr, err := erasure.DecodeShardHeader(shardFile)
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if _, err := shardFile.Seek(int64(m.Shards[0].PayloadLength), io.SeekCurrent); err != nil {
		t.Fatalf("seek payload: %v", err)
	}
	want, err := hex.DecodeString(m.Shards[0].ContentHashHex)
	if err != nil {
		t.Fatalf("decode manifest hash: %v", err)
	}
	if _, err := shardFile.Seek(int64(hdr.HeaderSize), io.SeekStart); err != nil {
		t.Fatalf("seek to payload: %v", err)
	}
	h := sha256.New()
	if _, err := io.CopyN(h, shardFile, int64(m.Shards[0].PayloadLength)); err != nil {
		t.Fatalf("rehash payload: %v", err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	if !bytes.Equal(sum[:], want) {
		payload := make([]byte, m.Shards[0].PayloadLength)
		if _, err := shardFile.ReadAt(payload, int64(hdr.HeaderSize)); err != nil {
			t.Fatalf("manifest hash mismatch got=%x want=%x (failed read: %v)", want, sum[:], err)
		}
		t.Fatalf("manifest hash mismatch got=%x want=%x payload=%q", want, sum[:], payload)
	}
	if _, err := shardFile.Seek(int64(hdr.HeaderSize)+int64(m.Shards[0].PayloadLength), io.SeekStart); err != nil {
		t.Fatalf("seek footer: %v", err)
	}
	footer, err := erasure.DecodeShardFooter(shardFile)
	if err != nil {
		t.Fatalf("decode footer: %v", err)
	}
	if !bytes.Equal(sum[:], footer.ContentHash[:]) {
		t.Fatalf("footer hash mismatch got=%x want=%x orig=%s", footer.ContentHash[:], sum[:], origHash)
	}

	// Verify GET succeeds after repair.
	if _, _, _, _, err := lfs.Get(ctx, "bkt", "obj.txt"); err != nil {
		t.Fatalf("Get after repair failed: %v", err)
	}
}

func TestSingleShardRewriterMissingManifestSkips(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rewriter := NewSingleShardRewriter(base)
	err := rewriter.Repair(ctx, RepairItem{
		Bucket: "missing",
		Key:    "none",
	})
	if err != ErrSkip {
		t.Fatalf("expected ErrSkip for missing manifest, got %v", err)
	}
}
