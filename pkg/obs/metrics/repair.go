package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"shardseal/pkg/repair"
)

// RepairMetrics exposes Prometheus collectors for the repair queue.
// It focuses on low-cardinality, cheap-to-collect series.
type RepairMetrics struct {
	reg        *prometheus.Registry
	queueDepth prometheus.Gauge
}

// NewRepairMetrics registers repair metrics on the provided registry.
func NewRepairMetrics(reg *prometheus.Registry) *RepairMetrics {
	qd := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "shardseal",
		Subsystem: "repair",
		Name:      "queue_depth",
		Help:      "Current number of items pending in the repair queue.",
	})
	_ = reg.Register(qd)
	return &RepairMetrics{
		reg:        reg,
		queueDepth: qd,
	}
}

// ObserveLen sets the queue depth gauge to n.
func (r *RepairMetrics) ObserveLen(n int) {
	r.queueDepth.Set(float64(n))
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