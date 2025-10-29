package storage

import (
	"context"
	"errors"
	"sync"
	"time"

	"s3free/pkg/erasure"
)

// Manifest describes object layout and integrity metadata for FreeXL v1.
// This is a scaffold and subject to evolution; keep additions backward-compatible.
type Manifest struct {
	Bucket       string
	Key          string
	VersionID    string
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time

	UserMetadata map[string]string

	// Parts contains per-part metadata (for single-part objects this is length 1).
	Parts []PartMeta

	// RS erasure coding parameters for this object.
	RS erasure.Params

	// Placement hint describing where shards reside (node/disk identifiers).
	Placement []ShardPlacement

	// Optional encryption metadata (SSE-S3/SSE-C scaffolding).
	Encryption *EncryptionInfo

	// Integrity anchors (e.g., Merkle roots) - placeholder for later.
	Integrity *IntegrityInfo
}

// PartMeta describes one logical object part.
type PartMeta struct {
	Index      int
	Size       int64
	ETag       string
	BlockCount int
}

// ShardPlacement identifies where a shard is stored.
type ShardPlacement struct {
	ShardIndex int
	NodeID     string
	DiskID     string
	Generation int64
}

// EncryptionInfo is a placeholder for encryption metadata.
type EncryptionInfo struct {
	KeyID     string
	Algorithm string
	Nonce     []byte
}

// IntegrityInfo is a placeholder for integrity metadata (e.g., Merkle root).
type IntegrityInfo struct {
	ObjectHashAlgo string
	ObjectMerkle   []byte
}

// ManifestStore abstracts storing and retrieving object manifests.
// Implementations MUST be concurrency-safe.
type ManifestStore interface {
	PutManifest(ctx context.Context, m Manifest) error
	GetManifest(ctx context.Context, bucket, key, versionID string) (Manifest, error)
	DeleteManifest(ctx context.Context, bucket, key, versionID string) error
}

// ErrManifestNotFound signals missing manifest.
var ErrManifestNotFound = errors.New("manifest: not found")

// MemManifestStore is an in-memory ManifestStore, suitable for tests and scaffolding.
// It is NOT durable and not for production use.
type MemManifestStore struct {
	mu   sync.RWMutex
	data map[string]Manifest
}

// NewMemManifestStore creates an empty in-memory manifest store.
func NewMemManifestStore() *MemManifestStore {
	return &MemManifestStore{data: make(map[string]Manifest)}
}

func (s *MemManifestStore) PutManifest(_ context.Context, m Manifest) error {
	if m.Bucket == "" || m.Key == "" {
		return errors.New("manifest: bucket/key required")
	}
	if m.VersionID == "" {
		// For scaffolding, default single version if absent.
		m.VersionID = "v1"
	}
	// Ensure LastModified set
	if m.LastModified.IsZero() {
		m.LastModified = time.Now().UTC()
	}
	k := makeKey(m.Bucket, m.Key, m.VersionID)
	s.mu.Lock()
	s.data[k] = m
	s.mu.Unlock()
	return nil
}

func (s *MemManifestStore) GetManifest(_ context.Context, bucket, key, versionID string) (Manifest, error) {
	if versionID == "" {
		versionID = "v1"
	}
	k := makeKey(bucket, key, versionID)
	s.mu.RLock()
	m, ok := s.data[k]
	s.mu.RUnlock()
	if !ok {
		return Manifest{}, ErrManifestNotFound
	}
	return m, nil
}

func (s *MemManifestStore) DeleteManifest(_ context.Context, bucket, key, versionID string) error {
	if versionID == "" {
		versionID = "v1"
	}
	k := makeKey(bucket, key, versionID)
	s.mu.Lock()
	_, ok := s.data[k]
	if ok {
		delete(s.data, k)
	}
	s.mu.Unlock()
	if !ok {
		return ErrManifestNotFound
	}
	return nil
}

func makeKey(bucket, key, versionID string) string {
	return bucket + "|" + key + "|" + versionID
}