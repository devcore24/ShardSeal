package erasure

import "errors"

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