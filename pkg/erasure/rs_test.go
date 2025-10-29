package erasure

import (
	"bytes"
	"testing"
)

func TestNoopCodec_ParamsAndEncode(t *testing.T) {
	p := Params{K: 3, M: 2, BlockSize: 4096, StripeSize: 4096 * 5}
	c, err := NewNoop(p)
	if err != nil {
		t.Fatalf("NewNoop: %v", err)
	}
	if got := c.Params(); got != p {
		t.Fatalf("Params mismatch: got=%+v want=%+v", got, p)
	}

	payload := []byte("hello-freexl")
	shards, err := c.Encode(payload)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(shards) != p.K+p.M {
		t.Fatalf("shard count: got=%d want=%d", len(shards), p.K+p.M)
	}
	// First K shards duplicate payload; parity shards are nil
	for i := 0; i < p.K; i++ {
		if !bytes.Equal(shards[i], payload) {
			t.Fatalf("data shard %d mismatch", i)
		}
	}
	for i := p.K; i < p.K+p.M; i++ {
		if shards[i] != nil {
			t.Fatalf("parity shard %d expected nil", i)
		}
	}
}

func TestNoopCodec_Reconstruct(t *testing.T) {
	p := Params{K: 2, M: 1}
	c, err := NewNoop(p)
	if err != nil {
		t.Fatalf("NewNoop: %v", err)
	}
	payload := []byte("freexl")
	shards, err := c.Encode(payload)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Lose shard 0
	shards[0] = nil
	out, err := c.Reconstruct(shards, []int{0})
	if err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if !bytes.Equal(out[0], payload) {
		t.Fatalf("reconstructed shard mismatch: got=%q want=%q", string(out[0]), string(payload))
	}
}

func TestNewNoop_ParamValidation(t *testing.T) {
	_, err := NewNoop(Params{K: 0, M: 1})
	if err == nil {
		t.Fatalf("expected error for K=0")
	}
	_, err = NewNoop(Params{K: 1, M: -1})
	if err == nil {
		t.Fatalf("expected error for M<0")
	}
}