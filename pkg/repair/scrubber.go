package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	erasure "shardseal/pkg/erasure"
	"shardseal/pkg/storage"
)

// Stats captures scrubber activity metrics.
type Stats struct {
	Scanned   uint64        // number of items scanned (objects/shards) since start
	Repaired  uint64        // number of repairs performed (no-ops in noop scrubber)
	Errors    uint64        // number of errors encountered
	LastRun   time.Time     // last completed run time
	LastError string        // last error string (if any)
	Uptime    time.Duration // total time since Start
}

// Config configures the scrubber behavior.
type Config struct {
	// Interval controls periodic scrub cadence when running background.
	Interval time.Duration
	// Concurrency controls parallelism of the repair workers.
	Concurrency int
	// VerifyPayload controls whether the scrubber recomputes sha256(payload)
	// and compares against the shard footer and manifest. When false, only
	// header/footer CRCs and footer vs. manifest hash are checked (faster).
	VerifyPayload bool
}

 // Scrubber defines the background scrubber interface for ShardSeal.
 // Implementations MUST be concurrency-safe.
type Scrubber interface {
	// Start launches background scrubbing until Stop is called or context is canceled.
	Start(ctx context.Context) error
	// Stop requests the background scrubbing to stop and waits for completion (with context timeout).
	Stop(ctx context.Context) error
	// RunOnce performs a single scrub pass synchronously (may be partial or sample-based).
	RunOnce(ctx context.Context) error
	// Stats returns a snapshot of the current stats.
	Stats() Stats
}

// NoopScrubber is a no-op implementation that updates counters and sleeps on interval.
// Useful for wiring, metrics, and integration testing. NOT for production.
type NoopScrubber struct {
	cfg   Config
	mu    sync.RWMutex
	start time.Time

	running atomic.Bool
	stopCh  chan struct{}
	doneCh  chan struct{}

	scanned   atomic.Uint64
	repaired  atomic.Uint64
	errors    atomic.Uint64
	lastRun   atomic.Pointer[time.Time]
	lastError atomic.Pointer[string]
}

// NewNoopScrubber creates a no-op scrubber with sane defaults.
func NewNoopScrubber(cfg Config) *NoopScrubber {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	return &NoopScrubber{
		cfg:   cfg,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

func (n *NoopScrubber) Start(ctx context.Context) error {
	if !n.running.CompareAndSwap(false, true) {
		return errors.New("scrubber: already running")
	}
	n.mu.Lock()
	n.start = time.Now()
	n.mu.Unlock()

	go n.loop(ctx)
	return nil
}

func (n *NoopScrubber) loop(ctx context.Context) {
	defer func() {
		n.running.Store(false)
		close(n.doneCh)
	}()
	t := time.NewTimer(n.cfg.Interval)
	defer t.Stop()

	// initial run immediately
	_ = n.doRunOnce(context.Background())

	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stopCh:
			return
		case <-t.C:
			_ = n.doRunOnce(context.Background())
			t.Reset(n.cfg.Interval)
		}
	}
}

func (n *NoopScrubber) Stop(ctx context.Context) error {
	if !n.running.Load() {
		return nil
	}
	select {
	case n.stopCh <- struct{}{}:
	default:
		// already signaled
	}
	select {
	case <-n.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (n *NoopScrubber) RunOnce(ctx context.Context) error {
	return n.doRunOnce(ctx)
}

func (n *NoopScrubber) doRunOnce(_ context.Context) error {
	// Simulate a scan cycle (no real work)
	n.scanned.Add(1)

	now := time.Now()
	n.lastRun.Store(&now)
	return nil
}

func (n *NoopScrubber) Stats() Stats {
	var lastRun time.Time
	if p := n.lastRun.Load(); p != nil {
		lastRun = *p
	}
	var lastErr string
	if e := n.lastError.Load(); e != nil {
		lastErr = *e
	}

	n.mu.RLock()
	start := n.start
	n.mu.RUnlock()

	return Stats{
		Scanned:   n.scanned.Load(),
		Repaired:  n.repaired.Load(),
		Errors:    n.errors.Load(),
		LastRun:   lastRun,
		LastError: lastErr,
		Uptime:    sinceIfSet(start),
	}
}

func sinceIfSet(t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}

// SealedScrubber verifies sealed objects on disk by reading manifests and shard files.
// It validates header/footer CRC32C and ensures the footer content hash matches the
// manifest. When VerifyPayload is enabled, it also recomputes sha256(payload) to
// detect latent corruption in the payload region. This implementation does not
// repair; it only detects and reports via Stats.
type SealedScrubber struct {
	cfg      Config
	baseDirs []string

	mu      sync.RWMutex
	start   time.Time
	running atomic.Bool
	stopCh  chan struct{}
	doneCh  chan struct{}

	scanned   atomic.Uint64
	repaired  atomic.Uint64
	errors    atomic.Uint64
	lastRun   atomic.Pointer[time.Time]
	lastError atomic.Pointer[string]
}

// NewSealedScrubber creates a scrubber for one or more ShardSeal data directories.
// Pass the same dataDirs you configure for LocalFS. Concurrency <= 0 defaults to 1.
func NewSealedScrubber(baseDirs []string, cfg Config) *SealedScrubber {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	return &SealedScrubber{
		cfg:      cfg,
		baseDirs: baseDirs,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

func (s *SealedScrubber) Start(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("scrubber: already running")
	}
	s.mu.Lock()
	s.start = time.Now()
	s.mu.Unlock()
	go s.loop(ctx)
	return nil
}

func (s *SealedScrubber) loop(ctx context.Context) {
	defer func() {
		s.running.Store(false)
		close(s.doneCh)
	}()
	// initial run
	_ = s.doRunOnce(context.Background())
	t := time.NewTimer(s.cfg.Interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
			_ = s.doRunOnce(context.Background())
			t.Reset(s.cfg.Interval)
		}
	}
}

func (s *SealedScrubber) Stop(ctx context.Context) error {
	if !s.running.Load() {
		return nil
	}
	select {
	case s.stopCh <- struct{}{}:
	default:
	}
	select {
	case <-s.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *SealedScrubber) RunOnce(ctx context.Context) error {
	return s.doRunOnce(ctx)
}

func (s *SealedScrubber) doRunOnce(ctx context.Context) error {
	var lastErrStr string
	err := s.scanAll(ctx)
	if err != nil {
		lastErrStr = err.Error()
	}
	now := time.Now()
	s.lastRun.Store(&now)
	if lastErrStr != "" {
		s.lastError.Store(&lastErrStr)
	}
	return err
}

func (s *SealedScrubber) Stats() Stats {
	var lastRun time.Time
	if p := s.lastRun.Load(); p != nil {
		lastRun = *p
	}
	var lastErr string
	if e := s.lastError.Load(); e != nil {
		lastErr = *e
	}
	s.mu.RLock()
	start := s.start
	s.mu.RUnlock()
	return Stats{
		Scanned:   s.scanned.Load(),
		Repaired:  s.repaired.Load(),
		Errors:    s.errors.Load(),
		LastRun:   lastRun,
		LastError: lastErr,
		Uptime:    sinceIfSet(start),
	}
}

func (s *SealedScrubber) scanAll(ctx context.Context) error {
	type job struct {
		base string
		bkt  string
		key  string
	}
	jobs := make(chan job, 1024)
	errCh := make(chan error, 1)

	// producer: walk manifests across all base dirs
	go func() {
		defer close(jobs)
		for _, base := range s.baseDirs {
			if base == "" {
				continue
			}
			root := filepath.Join(base, "objects")
			_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					// pass through walk errors to global errCh but keep walking
					select {
					case errCh <- err:
					default:
					}
					return nil
				}
				if d.IsDir() {
					return nil
				}
				if d.Name() != storage.ManifestFilename {
					return nil
				}
				// Derive bucket and key from path: base/objects/<bucket>/<key>/object.meta
				rel, rerr := filepath.Rel(base, p)
				if rerr != nil {
					return nil
				}
				relSlash := filepath.ToSlash(rel)
				if !strings.HasPrefix(relSlash, "objects/") {
					return nil
				}
				dir := filepath.ToSlash(filepath.Dir(relSlash)) // objects/<bucket>/<key>
				rest := strings.TrimPrefix(dir, "objects/")
				parts := strings.SplitN(rest, "/", 2)
				if len(parts) < 2 {
					return nil
				}
				bkt := parts[0]
				key := parts[1]
				select {
				case jobs <- job{base: base, bkt: bkt, key: key}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			})
		}
	}()

	// workers
	var wg sync.WaitGroup
	worker := func() {
		defer wg.Done()
		for j := range jobs {
			_ = s.verifyObject(ctx, j.base, j.bkt, j.key)
		}
	}
	for i := 0; i < s.cfg.Concurrency; i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (s *SealedScrubber) verifyObject(ctx context.Context, base, bucket, key string) error {
	// Load manifest
	m, err := storage.LoadManifest(ctx, base, bucket, key)
	if err != nil || m == nil || len(m.Shards) == 0 {
		return err
	}
	// Process all shards (M1: usually 1)
	var hadErr bool
	for _, sh := range m.Shards {
		sp := filepath.Join(base, filepath.FromSlash(sh.Path))
		f, oerr := os.Open(sp)
		if oerr != nil {
			hadErr = true
			s.errors.Add(1)
			continue
		}
		// Decode header
		if _, oerr = f.Seek(0, io.SeekStart); oerr != nil {
			_ = f.Close()
			hadErr = true
			s.errors.Add(1)
			continue
		}
		hdr, oerr := erasure.DecodeShardHeader(f)
		if oerr != nil {
			_ = f.Close()
			hadErr = true
			s.errors.Add(1)
			continue
		}
		// Read and verify footer CRC; compare content hash with manifest
		footerOff := int64(hdr.HeaderSize) + int64(hdr.PayloadLength)
		if _, oerr = f.Seek(footerOff, io.SeekStart); oerr != nil {
			_ = f.Close()
			hadErr = true
			s.errors.Add(1)
			continue
		}
		footer, oerr := erasure.DecodeShardFooter(f)
		if oerr != nil {
			_ = f.Close()
			hadErr = true
			s.errors.Add(1)
			continue
		}
		want, derr := hex.DecodeString(sh.ContentHashHex)
		if derr != nil || !bytesEqual(want, footer.ContentHash[:]) {
			_ = f.Close()
			hadErr = true
			s.errors.Add(1)
			continue
		}
		// Optionally recompute payload sha256 and compare with footer
		if s.cfg.VerifyPayload {
			if _, oerr = f.Seek(int64(hdr.HeaderSize), io.SeekStart); oerr != nil {
				_ = f.Close()
				hadErr = true
				s.errors.Add(1)
				continue
			}
			h := sha256.New()
			if _, oerr = io.CopyN(h, f, int64(hdr.PayloadLength)); oerr != nil {
				_ = f.Close()
				hadErr = true
				s.errors.Add(1)
				continue
			}
			var sum [32]byte
			copy(sum[:], h.Sum(nil))
			if !bytesEqual(sum[:], footer.ContentHash[:]) {
				_ = f.Close()
				hadErr = true
				s.errors.Add(1)
				continue
			}
		}
		_ = f.Close()
	}
	// Count one scanned object (aggregate over shards)
	s.scanned.Add(1)
	if hadErr {
		return errors.New("scrubber: one or more shard integrity checks failed")
	}
	return nil
}

// bytesEqual is a tiny helper avoiding importing bytes for one function.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}