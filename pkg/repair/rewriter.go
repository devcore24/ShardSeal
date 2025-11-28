package repair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	erasure "shardseal/pkg/erasure"
	"shardseal/pkg/storage"
)

var (
	// ErrSkip indicates the repair item could not be processed but should not be treated as a hard failure.
	ErrSkip = errors.New("repair: skipped")
	// ErrUnsupported indicates the repair item is incompatible with the current repair implementation.
	ErrUnsupported = errors.New("repair: unsupported item")
	// ErrPayloadMismatch indicates the shard payload hash does not match the manifest.
	ErrPayloadMismatch = errors.New("repair: payload hash mismatch")
)

// SingleShardRewriter attempts to repair single-shard sealed objects by rewriting their headers/footers.
// It operates directly on the LocalFS data directory.
type SingleShardRewriter struct {
	baseDir string
}

// NewSingleShardRewriter constructs a rewriter rooted at the provided data directory.
func NewSingleShardRewriter(baseDir string) *SingleShardRewriter {
	return &SingleShardRewriter{baseDir: baseDir}
}

// Repair validates the manifest/shard information and rewrites the sealed shard if the payload hash matches.
func (r *SingleShardRewriter) Repair(ctx context.Context, item RepairItem) error {
	if r == nil || r.baseDir == "" {
		return ErrSkip
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m, err := storage.LoadManifest(ctx, r.baseDir, item.Bucket, item.Key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrSkip
		}
		return fmt.Errorf("repair: load manifest: %w", err)
	}
	if len(m.Shards) != 1 {
		return ErrUnsupported
	}
	shard := m.Shards[0]
	if shard.Path == "" {
		return ErrUnsupported
	}
	if item.ShardPath != "" {
		want := filepath.ToSlash(item.ShardPath)
		if want != shard.Path {
			// Shard mismatch; skip to avoid touching unintended files.
			return ErrSkip
		}
	}
	payloadLen := shard.PayloadLength
	if payloadLen <= 0 {
		payloadLen = m.Size
	}
	if payloadLen <= 0 {
		return ErrUnsupported
	}
	const headerSize = 28
	const footerSize = 32 + 4
	absShard := filepath.Join(r.baseDir, filepath.FromSlash(shard.Path))
	info, err := os.Stat(absShard)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrSkip
		}
		return fmt.Errorf("repair: stat shard: %w", err)
	}
	expectedSize := int64(headerSize) + payloadLen + footerSize
	if info.Size() < expectedSize {
		return fmt.Errorf("repair: shard truncated (have %d, want %d)", info.Size(), expectedSize)
	}
	src, err := os.Open(absShard)
	if err != nil {
		return fmt.Errorf("repair: open shard: %w", err)
	}
	defer src.Close()
	if _, err := src.Seek(headerSize, io.SeekStart); err != nil {
		return fmt.Errorf("repair: seek payload: %w", err)
	}

	dir := filepath.Dir(absShard)
	tmp, err := os.CreateTemp(dir, ".repair-*")
	if err != nil {
		return fmt.Errorf("repair: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	hdrBytes, err := erasure.EncodeShardHeader(erasure.ShardHeader{
		Version:       1,
		HeaderSize:    0,
		PayloadLength: uint64(payloadLen),
	})
	if err != nil {
		return fmt.Errorf("repair: encode header: %w", err)
	}
	if _, err := tmp.Write(hdrBytes); err != nil {
		return fmt.Errorf("repair: write header: %w", err)
	}

	hasher := sha256.New()
	copied, err := io.CopyN(io.MultiWriter(tmp, hasher), src, payloadLen)
	if err != nil {
		return fmt.Errorf("repair: copy payload: %w", err)
	}
	if copied != payloadLen {
		return fmt.Errorf("repair: short payload: got %d want %d", copied, payloadLen)
	}
	var sum [32]byte
	copy(sum[:], hasher.Sum(nil))
	expectedHash, err := hex.DecodeString(shard.ContentHashHex)
	if err != nil || len(expectedHash) != len(sum) {
		return ErrUnsupported
	}
	if !bytes.Equal(sum[:], expectedHash) {
		return ErrPayloadMismatch
	}

	footerBytes, err := erasure.EncodeShardFooter(sum)
	if err != nil {
		return fmt.Errorf("repair: encode footer: %w", err)
	}
	if _, err := tmp.Write(footerBytes); err != nil {
		return fmt.Errorf("repair: write footer: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("repair: sync temp shard: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("repair: close temp shard: %w", err)
	}
	if err := os.Rename(tmpName, absShard); err != nil {
		return fmt.Errorf("repair: replace shard: %w", err)
	}
	if err := storage.SyncDir(dir); err != nil {
		return fmt.Errorf("repair: sync shard dir: %w", err)
	}

	// Update manifest CRC metadata to reflect new header/footer.
	if len(hdrBytes) >= 4 {
		shard.HeaderCRC32C = binary.LittleEndian.Uint32(hdrBytes[len(hdrBytes)-4:])
	}
	if len(footerBytes) >= 4 {
		shard.FooterCRC32C = binary.LittleEndian.Uint32(footerBytes[len(footerBytes)-4:])
	}
	shard.PayloadLength = payloadLen
	shard.ContentHashHex = hex.EncodeToString(sum[:])
	m.Shards[0] = shard
	m.LastModified = time.Now().UTC()
	if err := storage.SaveManifest(ctx, r.baseDir, item.Bucket, item.Key, m); err != nil {
		return fmt.Errorf("repair: save manifest: %w", err)
	}
	return nil
}
