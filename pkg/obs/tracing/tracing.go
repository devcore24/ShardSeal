package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
)

// Options controls tracing initialization.
type Options struct {
	Enabled     bool
	Endpoint    string  // OTLP collector endpoint (host:port or URL)
	Protocol    string  // "grpc" (default) or "http"
	SampleRatio float64 // 0.0 - 1.0
	ServiceName string  // default "s3free"
}

// Init configures OpenTelemetry tracing based on Options and sets global providers.
// It returns a shutdown function that should be called during graceful shutdown.
func Init(ctx context.Context, opt Options) (func(context.Context) error, error) {
	if !opt.Enabled {
		// No-op: ensure globals are in a sane state.
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{},
		))
		return func(context.Context) error { return nil }, nil
	}

	svc := opt.ServiceName
	if strings.TrimSpace(svc) == "" {
		svc = "s3free"
	}
	res, err := resource.New(ctx,
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithHost(),
		resource.WithAttributes(
			attribute.String("service.name", svc),
		),
	)
	if err != nil {
		// Proceed with minimal resource if creation fails.
		slog.Warn("tracing: resource init failed", slog.String("error", err.Error()))
		res = resource.Empty()
	}

	var exp sdktrace.SpanExporter
	if strings.TrimSpace(opt.Endpoint) != "" {
		switch strings.ToLower(strings.TrimSpace(opt.Protocol)) {
		case "http", "otlphttp", "otlp-http":
			httpOpts := []otlptracehttp.Option{
				otlptracehttp.WithEndpoint(stripScheme(opt.Endpoint)),
			}
			if isInsecure(opt.Endpoint) {
				httpOpts = append(httpOpts, otlptracehttp.WithInsecure())
			}
			xe, e := otlptracehttp.New(ctx, httpOpts...)
			if e != nil {
				slog.Error("tracing: otlp http exporter init failed", slog.String("error", e.Error()))
			} else {
				exp = xe
			}
		default: // grpc
			grpcOpts := []otlptracegrpc.Option{
				otlptracegrpc.WithEndpoint(stripScheme(opt.Endpoint)),
			}
			if isInsecure(opt.Endpoint) {
				grpcOpts = append(grpcOpts, otlptracegrpc.WithInsecure())
			}
			xe, e := otlptracegrpc.New(ctx, grpcOpts...)
			if e != nil {
				slog.Error("tracing: otlp grpc exporter init failed", slog.String("error", e.Error()))
			} else {
				exp = xe
			}
		}
	} else {
		slog.Info("tracing: enabled without endpoint; spans will not be exported")
	}

	// Sampler selection
	var sampler sdktrace.Sampler
	switch {
	case opt.SampleRatio >= 1.0:
		sampler = sdktrace.AlwaysSample()
	case opt.SampleRatio <= 0:
		sampler = sdktrace.NeverSample()
	default:
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(opt.SampleRatio))
	}

	// Build tracer provider
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}
	if exp != nil {
		opts = append(opts, sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithMaxExportBatchSize(512),
		))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	shutdown := func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}
	return shutdown, nil
}

// Middleware instruments incoming HTTP requests with a server span.
// It skips common health/metrics paths to reduce noise.
func Middleware(next http.Handler) http.Handler {
	skipped := map[string]struct{}{
		"/livez":   {},
		"/readyz":  {},
		"/metrics": {},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := skipped[r.URL.Path]; ok {
			next.ServeHTTP(w, r)
			return
		}

		tracer := otel.Tracer("s3free/http")
		ctx := r.Context()
		spanName := r.Method + " " + r.URL.EscapedPath()
		ctx, span := tracer.Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))
		elapsed := time.Since(start)

		// Minimal common HTTP attributes (avoid semconv dependency).
		span.SetAttributes(
			attribute.String("http.method", r.Method),
			attribute.String("http.target", r.URL.RequestURI()),
			attribute.String("http.route", r.URL.Path),
			attribute.Int("http.status_code", rec.status),
			attribute.String("net.peer.ip", clientIP(r)),
			attribute.String("user_agent.original", r.UserAgent()),
			attribute.Int64("http.server_duration_ms", elapsed.Milliseconds()),
		)
	})
}

// statusRecorder captures response status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Helpers

// isInsecure decides whether to use insecure transport based on endpoint hints.
func isInsecure(endpoint string) bool {
	ep := strings.ToLower(strings.TrimSpace(endpoint))
	if strings.HasPrefix(ep, "http://") {
		return true
	}
	// Heuristic for local dev.
	if strings.Contains(ep, "localhost") || strings.Contains(ep, "127.0.0.1") {
		return true
	}
	return false
}

// stripScheme removes URL scheme to fit OTLP client expectations when necessary.
func stripScheme(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if strings.HasPrefix(strings.ToLower(e), "http://") {
		return strings.TrimPrefix(e, "http://")
	}
	if strings.HasPrefix(strings.ToLower(e), "https://") {
		return strings.TrimPrefix(e, "https://")
	}
	return e
}

// clientIP extracts a best-effort client IP from request.
func clientIP(r *http.Request) string {
	// Prefer X-Forwarded-For (first entry) if present.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	if ra := r.RemoteAddr; ra != "" {
		return ra
	}
	return ""
}