package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// StorageMetrics holds Prometheus collectors for storage-layer instrumentation.
type StorageMetrics struct {
	reg     *prometheus.Registry
	bytes   *prometheus.CounterVec
	ops     *prometheus.CounterVec
	latency *prometheus.HistogramVec
}

// StorageObserver is a minimal observer interface implemented by StorageMetrics.
type StorageObserver interface {
	Observe(op string, bytes int64, err error, dur time.Duration)
}

// NewStorageMetrics registers storage metrics on the provided registry.
func NewStorageMetrics(reg *prometheus.Registry) *StorageMetrics {
	bytes := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "s3free",
		Subsystem: "storage",
		Name:      "bytes_total",
		Help:      "Total bytes processed by storage operations.",
	}, []string{"op"})
	ops := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "s3free",
		Subsystem: "storage",
		Name:      "ops_total",
		Help:      "Total number of storage operations by result.",
	}, []string{"op", "result"}) // result = "ok" | "error"
	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "s3free",
		Subsystem: "storage",
		Name:      "op_duration_seconds",
		Help:      "Histogram of storage operation durations in seconds.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"op"})

	_ = reg.Register(bytes)
	_ = reg.Register(ops)
	_ = reg.Register(latency)

	return &StorageMetrics{
		reg:     reg,
		bytes:   bytes,
		ops:     ops,
		latency: latency,
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