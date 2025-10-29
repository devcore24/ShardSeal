package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics provides a self-contained Prometheus registry, common HTTP metrics,
// and helpers to expose /metrics and instrument handlers without creating import cycles.
type Metrics struct {
	reg      *prometheus.Registry
	inflight prometheus.Gauge
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
}

// New creates a Metrics instance with a fresh registry and registers collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	inflight := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "s3free",
		Subsystem: "http",
		Name:      "inflight_requests",
		Help:      "Current number of inflight HTTP requests.",
	})
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "s3free",
		Subsystem: "http",
		Name:      "requests_total",
		Help:      "Total number of HTTP requests processed, partitioned by status code and method.",
	}, []string{"code", "method"})
	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "s3free",
		Subsystem: "http",
		Name:      "request_duration_seconds",
		Help:      "Histogram of latencies for HTTP requests.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"code", "method"})

	_ = reg.Register(inflight)
	_ = reg.Register(requests)
	_ = reg.Register(latency)

	return &Metrics{
		reg:      reg,
		inflight: inflight,
		requests: requests,
		latency:  latency,
	}
}

// Handler returns an http.Handler that serves Prometheus metrics using the internal registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// statusRecorder captures the HTTP status code written by a handler.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Middleware wraps an http.Handler to collect basic HTTP metrics:
// - inflight gauge
// - requests_total counter (labels: method, code)
// - request_duration_seconds histogram (labels: method, code)
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.inflight.Inc()
		defer m.inflight.Dec()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		method := r.Method
		code := strconv.Itoa(rec.status)
		elapsed := time.Since(start).Seconds()

		m.requests.WithLabelValues(code, method).Inc()
		m.latency.WithLabelValues(code, method).Observe(elapsed)
	})
}

// Registry returns the underlying Prometheus registry for advanced usage.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.reg
}