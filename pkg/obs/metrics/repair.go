package metrics

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"shardseal/pkg/repair"
)

// RepairMetrics exposes Prometheus collectors for the repair queue.
// It focuses on low-cardinality, cheap-to-collect series.
type RepairMetrics struct {
	reg        *prometheus.Registry
	queueDepth prometheus.Gauge
	enqueued   *prometheus.CounterVec
	completed  *prometheus.CounterVec
	duration   *prometheus.HistogramVec
}

// NewRepairMetrics registers repair metrics on the provided registry.
func NewRepairMetrics(reg *prometheus.Registry) *RepairMetrics {
	qd := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "shardseal",
		Subsystem: "repair",
		Name:      "queue_depth",
		Help:      "Current number of items pending in the repair queue.",
	})
	enq := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "shardseal",
		Subsystem: "repair",
		Name:      "enqueued_total",
		Help:      "Total repair items enqueued, labeled by reason.",
	}, []string{"reason"})
	comp := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "shardseal",
		Subsystem: "repair",
		Name:      "completed_total",
		Help:      "Total repair items processed by result.",
	}, []string{"result"})
	dur := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "shardseal",
		Subsystem: "repair",
		Name:      "duration_seconds",
		Help:      "Histogram of repair execution durations by result.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"result"})
	_ = reg.Register(qd)
	_ = reg.Register(enq)
	_ = reg.Register(comp)
	_ = reg.Register(dur)
	return &RepairMetrics{
		reg:        reg,
		queueDepth: qd,
		enqueued:   enq,
		completed:  comp,
		duration:   dur,
	}
}

// ObserveLen sets the queue depth gauge to n.
func (r *RepairMetrics) ObserveLen(n int) {
	r.queueDepth.Set(float64(n))
}

// ObserveEnqueue increments the enqueued counter for the provided reason.
func (r *RepairMetrics) ObserveEnqueue(reason string) {
	if r == nil {
		return
	}
	r.enqueued.WithLabelValues(normalizeReason(reason)).Inc()
}

// ObserveResult records a completed repair with the given result label and duration.
func (r *RepairMetrics) ObserveResult(result string, dur time.Duration) {
	if r == nil {
		return
	}
	lbl := normalizeResult(result)
	r.completed.WithLabelValues(lbl).Inc()
	r.duration.WithLabelValues(lbl).Observe(dur.Seconds())
}

// StartPolling periodically observes q.Len() and updates the gauge.
// Returns a stop function to cancel the poller.
func (r *RepairMetrics) StartPolling(q repair.RepairQueue, interval time.Duration) (stop func()) {
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
				if q != nil {
					r.ObserveLen(q.Len())
				}
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

func normalizeReason(reason string) string {
	reason = strings.TrimSpace(strings.ToLower(reason))
	if reason == "" {
		return "unknown"
	}
	return reason
}

func normalizeResult(result string) string {
	result = strings.TrimSpace(strings.ToLower(result))
	if result == "" {
		return "unknown"
	}
	return result
}
