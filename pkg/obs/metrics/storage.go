package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// StorageMetrics holds Prometheus collectors for storage-layer instrumentation.
type StorageMetrics struct {
	reg           *prometheus.Registry
	bytes         *prometheus.CounterVec
	ops           *prometheus.CounterVec
	latency       *prometheus.HistogramVec
	// Sealed I/O specific metrics
	sealedOps     *prometheus.CounterVec      // labels: op, sealed, result, integrity_fail
	sealedLatency *prometheus.HistogramVec    // labels: op, sealed, integrity_fail
	integrityFail *prometheus.CounterVec      // labels: op
}

// StorageObserver is a minimal observer interface implemented by StorageMetrics.
type StorageObserver interface {
	Observe(op string, bytes int64, err error, dur time.Duration)
}

// NewStorageMetrics registers storage metrics on the provided registry.
func NewStorageMetrics(reg *prometheus.Registry) *StorageMetrics {
	bytes := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "shardseal",
		Subsystem: "storage",
		Name:      "bytes_total",
		Help:      "Total bytes processed by storage operations.",
	}, []string{"op"})
	ops := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "shardseal",
		Subsystem: "storage",
		Name:      "ops_total",
		Help:      "Total number of storage operations by result.",
	}, []string{"op", "result"}) // result = "ok" | "error"
	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "shardseal",
		Subsystem: "storage",
		Name:      "op_duration_seconds",
		Help:      "Histogram of storage operation durations in seconds.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"op"})

	// Sealed I/O metrics
	sealedOps := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "shardseal",
		Subsystem: "storage",
		Name:      "sealed_ops_total",
		Help:      "Total storage operations with sealed metadata.",
	}, []string{"op", "sealed", "result", "integrity_fail"})
	sealedLatency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "shardseal",
		Subsystem: "storage",
		Name:      "sealed_op_duration_seconds",
		Help:      "Histogram of durations for storage operations with sealed metadata.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"op", "sealed", "integrity_fail"})
	integrityFail := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "shardseal",
		Subsystem: "storage",
		Name:      "integrity_failures_total",
		Help:      "Number of integrity verification failures detected in sealed I/O.",
	}, []string{"op"})

	_ = reg.Register(bytes)
	_ = reg.Register(ops)
	_ = reg.Register(latency)
	_ = reg.Register(sealedOps)
	_ = reg.Register(sealedLatency)
	_ = reg.Register(integrityFail)

	return &StorageMetrics{
		reg:           reg,
		bytes:         bytes,
		ops:           ops,
		latency:       latency,
		sealedOps:     sealedOps,
		sealedLatency: sealedLatency,
		integrityFail: integrityFail,
	}
}

// Observe records a storage operation with optional bytes and error.
// dur must be the total time spent in the operation.
func (m *StorageMetrics) Observe(op string, bytes int64, err error, dur time.Duration) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	if bytes > 0 {
		m.bytes.WithLabelValues(op).Add(float64(bytes))
	}
	m.ops.WithLabelValues(op, result).Inc()
	m.latency.WithLabelValues(op).Observe(dur.Seconds())
}

// ObserveSealed records sealed-mode specific metrics alongside the generic ones.
// sealed indicates whether the op used sealed I/O; integrityFail marks integrity verification failures.
func (m *StorageMetrics) ObserveSealed(op string, bytes int64, err error, dur time.Duration, sealed bool, integrityFail bool) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	s := "false"
	if sealed {
		s = "true"
	}
	intf := "false"
	if integrityFail {
		intf = "true"
	}
	// Reuse base series
	if bytes > 0 {
		m.bytes.WithLabelValues(op).Add(float64(bytes))
	}
	m.ops.WithLabelValues(op, result).Inc()
	m.latency.WithLabelValues(op).Observe(dur.Seconds())

	// Sealed series
	m.sealedOps.WithLabelValues(op, s, result, intf).Inc()
	m.sealedLatency.WithLabelValues(op, s, intf).Observe(dur.Seconds())
	if integrityFail {
		m.integrityFail.WithLabelValues(op).Inc()
	}
}