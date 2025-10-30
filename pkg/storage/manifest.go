package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ManifestFilename is the filename used to persist object manifests alongside data.
const ManifestFilename = "object.meta"

 // ManifestFormatV1 identifies the v1 JSON encoding format for manifests (ShardSeal v1).
const ManifestFormatV1 = "ShardSealv1"

// RSParams describes Reed–Solomon parameters for an object.
type RSParams struct {
	K          int   `json:"k"`           // data shards
	M          int   `json:"m"`           // parity shards
	BlockSize  int64 `json:"block_size"`  // bytes per block before RS
	StripeSize int64 `json:"stripe_size"` // total per stripe (derived from K+M and BlockSize)
}

// PartMeta describes a single part of a (possibly multipart) object.
type PartMeta struct {
	Index      int    `json:"index"`
	Size       int64  `json:"size"`
	ETag       string `json:"etag"`
	BlockCount int    `json:"block_count"`
}

// IntegrityInfo stores object-level integrity data.
type IntegrityInfo struct {
	ObjectMerkleRoot string `json:"object_merkle_root,omitempty"`
	HashAlgo         string `json:"hash_algo,omitempty"` // e.g., "sha256", "blake3"
}

// Manifest is the persisted description of an object, its layout, and integrity metadata.
// Format is JSON (v1). Later versions may switch to Protobuf; version is recorded in Version field.
type Manifest struct {
	Version      string        `json:"version"` // "ShardSealv1"
	Bucket       string        `json:"bucket"`
	Key          string        `json:"key"`
	Size         int64         `json:"size"`
	ETag         string        `json:"etag"`
	ContentType  string        `json:"content_type,omitempty"`
	LastModified time.Time     `json:"last_modified"`
	Parts        []PartMeta    `json:"parts,omitempty"`
	RS           RSParams      `json:"rs_params"`
	Integrity    IntegrityInfo `json:"integrity,omitempty"`
	// Future fields: placement, encryption, custom metadata, version IDs, etc.
}

// ValidateBasic performs minimal structural validation on the manifest.
func (m *Manifest) ValidateBasic() error {
	if m == nil {
		return errors.New("manifest nil")
	}
	if m.Version != ManifestFormatV1 {
		return fmt.Errorf("invalid manifest version %q", m.Version)
	}
	if m.Bucket == "" || m.Key == "" {
		return errors.New("manifest requires bucket and key")
	}
	if m.Size < 0 {
		return errors.New("manifest size negative")
	}
	if m.RS.K <= 0 || m.RS.M < 0 {
		return errors.New("invalid RS params")
	}
	return nil
}

// manifestPath returns the absolute path to the manifest file given a base data directory.
// Layout mirrors LocalFS objects layout: <base>/objects/<bucket>/<key>/object.meta
func manifestPath(baseDir, bucket, key string) (string, error) {
	if baseDir == "" {
		return "", errors.New("baseDir empty")
	}
	cleanKey := filepath.ToSlash(filepath.Clean("/" + key))[1:]
	p := filepath.Join(baseDir, "objects", bucket, cleanKey, ManifestFilename)
	return p, nil
}

// SaveManifest writes the manifest atomically under the given base directory.
// The write is done by writing a temp file in the target directory and renaming it.
func SaveManifest(ctx context.Context, baseDir, bucket, key string, m *Manifest) error {
	if m == nil {
		return errors.New("nil manifest")
	}
	if m.Version == "" {
		m.Version = ManifestFormatV1
	}
	if m.LastModified.IsZero() {
		m.LastModified = time.Now().UTC()
	}
	if err := m.ValidateBasic(); err != nil {
		return err
	}
	dst, err := manifestPath(baseDir, bucket, key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// Marshal JSON (compact to reduce overhead; indent for readability if preferred)
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".object.meta.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	// Write
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// fsync to be safer on crashes
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Atomic rename
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	// Best-effort timestamp
	_ = os.Chtimes(dst, time.Now(), m.LastModified)
	return nil
}

// LoadManifest reads and unmarshals the manifest for an object.
func LoadManifest(ctx context.Context, baseDir, bucket, key string) (*Manifest, error) {
	path, err := manifestPath(baseDir, bucket, key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var m Manifest
	dec := json.NewDecoder(f)
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	// Basic validation
	if err := m.ValidateBasic(); err != nil {
		return nil, err
	}
	return &m, nil
}