package erasure

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

// Params describes Reed–Solomon (RS) erasure coding parameters for ShardSeal.
// K = data shards, M = parity shards. BlockSize and StripeSize are advisory
// and may be used by concrete codecs; Noop ignores them.
type Params struct {
	K          int
	M          int
	BlockSize  int
	StripeSize int
}

// Codec is the minimal interface an erasure codec must satisfy for ShardSeal.
// Implementations MUST be concurrency-safe.
type Codec interface {
	Params() Params
	// Encode splits data into K data shards and M parity shards.
	// Noop returns K identical data shards and zero-length parity shards.
	Encode(data []byte) (shards [][]byte, err error)
	// Reconstruct fills missing shard indices using surviving shards.
	Reconstruct(shards [][]byte, missing []int) ([][]byte, error)
}

// NoopCodec is a placeholder that does not perform real erasure coding.
// It duplicates the full payload across K data shards and provides M empty parity shards.
// DO NOT USE IN PRODUCTION.
type NoopCodec struct{ p Params }

// NewNoop creates a NoopCodec after validating parameters.
func NewNoop(p Params) (*NoopCodec, error) {
	if p.K <= 0 {
		return nil, errors.New("erasure: K must be > 0")
	}
	if p.M < 0 {
		return nil, errors.New("erasure: M must be >= 0")
	}
	return &NoopCodec{p: p}, nil
}

func (c *NoopCodec) Params() Params { return c.p }

func (c *NoopCodec) Encode(data []byte) ([][]byte, error) {
	shards := make([][]byte, c.p.K+c.p.M)
	for i := 0; i < c.p.K; i++ {
		// duplicate payload for data shards
		cp := make([]byte, len(data))
		copy(cp, data)
		shards[i] = cp
	}
	// parity shards empty
	for i := c.p.K; i < c.p.K+c.p.M; i++ {
		shards[i] = nil
	}
	return shards, nil
}

func (c *NoopCodec) Reconstruct(shards [][]byte, missing []int) ([][]byte, error) {
	if len(shards) != c.p.K+c.p.M {
		return nil, errors.New("erasure: invalid shard count")
	}
	// find a non-nil data shard
	var donor []byte
	for i := 0; i < c.p.K; i++ {
		if len(shards[i]) > 0 {
			donor = shards[i]
			break
		}
	}
	if donor == nil {
		return nil, errors.New("erasure: cannot reconstruct without any surviving data shard")
	}
	// reconstruct missing data shards from donor; leave parity as-is
	out := make([][]byte, len(shards))
	copy(out, shards)
	for _, idx := range missing {
		if idx < 0 || idx >= len(out) {
			return nil, errors.New("erasure: missing index out of range")
		}
		if idx < c.p.K {
			cp := make([]byte, len(donor))
			copy(cp, donor)
			out[idx] = cp
		}
	}
	return out, nil
}

// ShardSeal v1 sealed shard header/footer encoding (stream-friendly)

// shardSealMagic identifies ShardSeal v1 in the header prefix.
const shardSealMagic = "ShardSealv1"

// shardHeaderVersion is the current header version.
const shardHeaderVersion uint16 = 1

var le = binary.LittleEndian
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// ShardHeader is the minimal header for M1 (single-shard, no RS striping).
// Layout on disk (little-endian):
//   magic[12] | version:u16 | headerSize:u16 | payloadLen:u64 | headerCRC32C:u32
type ShardHeader struct {
	Version       uint16
	HeaderSize    uint16
	PayloadLength uint64
}

// ShardFooter is the minimal footer for M1.
// Layout on disk:
//   contentHash[32] (sha256 for M1) | footerCRC32C:u32
type ShardFooter struct {
	ContentHash [32]byte
}

// EncodeShardHeader returns the binary header including CRC32C.
func EncodeShardHeader(h ShardHeader) ([]byte, error) {
	// Default to the v1 header length (28 bytes) when not provided.
	if h.HeaderSize == 0 {
		h.HeaderSize = 28
	}
	const minHeaderLen = len(shardSealMagic) + 2 + 2 + 8 + 4
	if int(h.HeaderSize) < minHeaderLen {
		return nil, errors.New("shard: header size too small")
	}
	buf := new(bytes.Buffer)
	// magic
	if _, err := buf.Write([]byte(shardSealMagic)); err != nil {
		return nil, err
	}
	// fields preceding CRC
	if err := binary.Write(buf, le, h.Version); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, le, h.HeaderSize); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, le, h.PayloadLength); err != nil {
		return nil, err
	}
	crc := crc32.Checksum(buf.Bytes(), crcTable)
	if err := binary.Write(buf, le, crc); err != nil {
		return nil, err
	}
	// Pad to HeaderSize so the encoded bytes match the advertised length.
	if pad := int(h.HeaderSize) - buf.Len(); pad > 0 {
		// Pad with zero bytes; payload readers skip HeaderSize bytes regardless.
		if _, err := buf.Write(make([]byte, pad)); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// DecodeShardHeader reads and verifies a header from r.
func DecodeShardHeader(r io.Reader) (ShardHeader, error) {
	var h ShardHeader
	// read magic
	magic := make([]byte, len(shardSealMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return h, err
	}
	if string(magic) != shardSealMagic {
		return h, errors.New("shard: invalid magic")
	}
	// buffer the remaining fixed fields to verify CRC
	payload := new(bytes.Buffer)
	// version, size, len
	var ver uint16
	var hsize uint16
	var plen uint64
	if err := binary.Read(r, le, &ver); err != nil {
		return h, err
	}
	if err := binary.Read(r, le, &hsize); err != nil {
		return h, err
	}
	if err := binary.Read(r, le, &plen); err != nil {
		return h, err
	}
	// reconstruct bytes for CRC
	payload.Write([]byte(shardSealMagic))
	_ = binary.Write(payload, le, ver)
	_ = binary.Write(payload, le, hsize)
	_ = binary.Write(payload, le, plen)

	// read stored crc
	var stored uint32
	if err := binary.Read(r, le, &stored); err != nil {
		return h, err
	}
	computed := crc32.Checksum(payload.Bytes(), crcTable)
	if stored != computed {
		return h, errors.New("shard: header CRC32C mismatch")
	}
	h.Version = ver
	h.HeaderSize = hsize
	h.PayloadLength = plen
	return h, nil
}

// EncodeShardFooter returns the binary footer including CRC32C.
func EncodeShardFooter(contentHash [32]byte) ([]byte, error) {
	buf := new(bytes.Buffer)
	if _, err := buf.Write(contentHash[:]); err != nil {
		return nil, err
	}
	crc := crc32.Checksum(buf.Bytes(), crcTable)
	if err := binary.Write(buf, le, crc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeShardFooter reads and verifies a footer from r.
func DecodeShardFooter(r io.Reader) (ShardFooter, error) {
	var f ShardFooter
	// read contentHash
	if _, err := io.ReadFull(r, f.ContentHash[:]); err != nil {
		return f, err
	}
	// buffer for CRC computation
	payload := f.ContentHash[:]
	var stored uint32
	if err := binary.Read(r, le, &stored); err != nil {
		return f, err
	}
	computed := crc32.Checksum(payload, crcTable)
	if stored != computed {
		return f, errors.New("shard: footer CRC32C mismatch")
	}
	return f, nil
}

// HashPayloadSHA256 streams from r and returns sha256 sum and bytes copied into w (if non-nil).
// This is a helper for future wiring; safe to keep here for now.
func HashPayloadSHA256(r io.Reader, w io.Writer) ([32]byte, int64, error) {
	h := sha256.New()
	var n int64
	var err error
	if w != nil {
		n, err = io.Copy(io.MultiWriter(h, w), r)
	} else {
		n, err = io.Copy(h, r)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, n, err
}
