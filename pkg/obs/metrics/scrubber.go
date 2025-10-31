package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"shardseal/pkg/repair"
)

// ScrubberMetrics exposes Prometheus collectors for the integrity scrubber.
type ScrubberMetrics struct {
	reg        *prometheus.Registry
	scanned    prometheus.Counter
	errors     prometheus.Counter
	lastRun    prometheus.Gauge
	uptime     prometheus.Gauge
}

// NewScrubberMetrics registers scrubber metrics on the provided registry.
func NewScrubberMetrics(reg *prometheus.Registry) *ScrubberMetrics {
	scanned := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "shardseal",
		Subsystem: "scrubber",
		Name:      "scanned_total",
		Help:      "Total number of objects/shards scanned by the scrubber since start.",
	})
	errors := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "shardseal",
		Subsystem: "scrubber",
		Name:      "errors_total",
		Help:      "Total number of scrubber errors detected since start.",
	})
	lastRun := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "shardseal",
		Subsystem: "scrubber",
		Name:      "last_run_timestamp_seconds",
		Help:      "Timestamp of the last completed scrub pass in seconds since epoch.",
	})
	uptime := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "shardseal",
		Subsystem: "scrubber",
		Name:      "uptime_seconds",
		Help:      "Total time in seconds since the scrubber was started.",
	})

	_ = reg.Register(scanned)
	_ = reg.Register(errors)
	_ = reg.Register(lastRun)
	_ = reg.Register(uptime)

	return &ScrubberMetrics{
		reg:     reg,
		scanned: scanned,
		errors:  errors,
		lastRun: lastRun,
		uptime:  uptime,
	}
}

// Observe updates metrics based on a Stats snapshot.
// This method is safe to call periodically (e.g., every 10s).
func (s *ScrubberMetrics) Observe(st repair.Stats) {
	// Convert counters: since Prometheus Counter only increments, compute deltas by setting absolute via Add(delta).
	// To keep it simple with atomic Stats (since this Observe may be called repeatedly),
	// we set the metrics to the absolute values only by tracking previous values internally.
	// For now, we use direct Set on Gauges for absolute values and Convert Counters using Add with deltas.

	// Use gauges for absolute counts to avoid complexity (prometheus allows counters only increasing).
	// However, changing types now would require re-registering. Instead, we provide idempotent behavior:
	// We keep internal static variables to hold last values. This avoids multiple structs or locks;
	// package-level state is acceptable for this small helper.

	addDelta(&prevScanned, float64(st.Scanned), s.scanned)
	addDelta(&prevErrors, float64(st.Errors), s.errors)

	if !st.LastRun.IsZero() {
		s.lastRun.Set(float64(st.LastRun.Unix()))
	}
	s.uptime.Set(st.Uptime.Seconds())
}

// Simple delta tracker (package-level), sufficient for single-process exporter.
var (
	prevScanned float64
	prevErrors  float64
)

func addDelta(prev *float64, current float64, c prometheus.Counter) {
	delta := current - *prev
	if delta < 0 {
		// Reset (process restarted or counters reset). Start from current.
		*prev = current
		return
	}
	if delta > 0 {
		c.Add(delta)
		*prev = current
	}
}

// StartPolling attaches a periodic poller that reads scr.Stats() at the given interval
// and pushes into metrics via Observe. Returns a stop function to cancel the poller.
func (s *ScrubberMetrics) StartPolling(scr repair.Scrubber, interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				st := scr.Stats()
				s.Observe(st)
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}