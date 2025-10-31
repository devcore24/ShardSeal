package erasure

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
)

func TestShardHeader_EncodeDecode_RoundTrip(t *testing.T) {
	h := ShardHeader{
		Version:       shardHeaderVersion,
		HeaderSize:    0,        // let encoder fill
		PayloadLength: 12345,    // arbitrary
	}
	enc, err := EncodeShardHeader(h)
	if err != nil {
		t.Fatalf("EncodeShardHeader: %v", err)
	}
	if len(enc) == 0 {
		t.Fatalf("empty encoded header")
	}
	dec, err := DecodeShardHeader(bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("DecodeShardHeader: %v", err)
	}
	if dec.Version != h.Version {
		t.Fatalf("version mismatch: got=%d want=%d", dec.Version, h.Version)
	}
	if dec.PayloadLength != h.PayloadLength {
		t.Fatalf("payload length mismatch: got=%d want=%d", dec.PayloadLength, h.PayloadLength)
	}
	if dec.HeaderSize == 0 {
		t.Fatalf("header size not set by encoder")
	}
}

func TestShardHeader_DetectsTamper(t *testing.T) {
	h := ShardHeader{
		Version:       shardHeaderVersion,
		HeaderSize:    0,
		PayloadLength: 999,
	}
	enc, err := EncodeShardHeader(h)
	if err != nil {
		t.Fatalf("EncodeShardHeader: %v", err)
	}
	// Tamper with one byte in the header payload (e.g., change payload length byte)
	enc2 := append([]byte(nil), enc...)
	// Ensure we flip a byte within the pre-CRC region (after magic): magic(len), ver(2), size(2), len(8)
	// Flip last byte of payload length to trigger CRC mismatch
	const verSize = 2
	const sizeSize = 2
	const lenSize = 8
	const crcSize = 4
	headerTotal := len(shardSealMagic) + verSize + sizeSize + lenSize + crcSize
	if len(enc2) < headerTotal {
		t.Fatalf("unexpected header size: %d (expected at least %d)", len(enc2), headerTotal)
	}
	idx := len(shardSealMagic) + verSize + sizeSize + lenSize - 1
	enc2[idx] ^= 0xFF

	if _, err := DecodeShardHeader(bytes.NewReader(enc2)); err == nil {
		t.Fatalf("expected CRC mismatch error, got nil")
	}
}

func TestShardFooter_EncodeDecode_RoundTrip(t *testing.T) {
	payload := []byte("hello footer")
	sum := sha256.Sum256(payload)

	enc, err := EncodeShardFooter(sum)
	if err != nil {
		t.Fatalf("EncodeShardFooter: %v", err)
	}
	f, err := DecodeShardFooter(bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("DecodeShardFooter: %v", err)
	}
	if f.ContentHash != sum {
		t.Fatalf("footer content hash mismatch")
	}
}

func TestShardFooter_DetectsTamper(t *testing.T) {
	payload := []byte("tamper check")
	sum := sha256.Sum256(payload)
	enc, err := EncodeShardFooter(sum)
	if err != nil {
		t.Fatalf("EncodeShardFooter: %v", err)
	}
	enc2 := append([]byte(nil), enc...)
	// Tamper any byte in contentHash region
	if len(enc2) < 32+4 {
		t.Fatalf("unexpected footer size: %d", len(enc2))
	}
	enc2[0] ^= 0x01
	if _, err := DecodeShardFooter(bytes.NewReader(enc2)); err == nil {
		t.Fatalf("expected footer CRC mismatch error, got nil")
	}
}

func TestHashPayloadSHA256(t *testing.T) {
	data := []byte("The quick brown fox jumps over the lazy dog")
	wantSum := sha256.Sum256(data)
	var buf bytes.Buffer

	sum, n, err := HashPayloadSHA256(bytes.NewReader(data), &buf)
	if err != nil {
		t.Fatalf("HashPayloadSHA256: %v", err)
	}
	if n != int64(len(data)) {
		t.Fatalf("bytes copied mismatch: got=%d want=%d", n, len(data))
	}
	if !bytes.Equal(sum[:], wantSum[:]) {
		t.Fatalf("hash mismatch: got=%s want=%s", hex.EncodeToString(sum[:]), hex.EncodeToString(wantSum[:]))
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("writer did not receive identical payload")
	}
}

func TestHashPayloadSHA256_NoWriter(t *testing.T) {
	data := []byte("payload only hash")
	wantSum := sha256.Sum256(data)

	sum, n, err := HashPayloadSHA256(bytes.NewReader(data), nil)
	if err != nil && err != io.EOF { // io.Copy may return nil err here
		t.Fatalf("HashPayloadSHA256 (no writer): %v", err)
	}
	if n != int64(len(data)) {
		t.Fatalf("bytes copied mismatch: got=%d want=%d", n, len(data))
	}
	if sum != wantSum {
		t.Fatalf("hash mismatch: got=%s want=%s", hex.EncodeToString(sum[:]), hex.EncodeToString(wantSum[:]))
	}
}