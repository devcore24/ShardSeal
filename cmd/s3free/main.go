package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"s3free/pkg/api/s3"
	"s3free/pkg/config"
	"s3free/pkg/metadata"
	"s3free/pkg/obs/metrics"
	"s3free/pkg/obs/tracing"
	"s3free/pkg/security/sigv4"
	"s3free/pkg/storage"
)

var version = "0.0.1-dev"
var ready atomic.Bool

func main() {
	// Load config from S3FREE_CONFIG or ./config.yaml; defaults otherwise.
	cfgPath := os.Getenv("S3FREE_CONFIG")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", slog.String("error", err.Error()))
		os.Exit(1)
	}
	// Ensure data directories exist.
	if err := config.EnsureDirs(cfg); err != nil {
		slog.Error("failed to ensure data dirs", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Initialize tracing (OpenTelemetry)
	traceShutdown, terr := tracing.Init(context.Background(), tracing.Options{
		Enabled:     cfg.Tracing.Enabled,
		Endpoint:    cfg.Tracing.Endpoint,
		Protocol:    cfg.Tracing.Protocol,
		SampleRatio: cfg.Tracing.SampleRatio,
		ServiceName: cfg.Tracing.ServiceName,
	})
	if terr != nil {
		slog.Warn("tracing init failed", slog.String("error", terr.Error()))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	// Metrics: Prometheus /metrics endpoint and HTTP instrumentation
	m := metrics.New()
	mux.Handle("/metrics", m.Handler())
	// Register storage-level metrics on shared registry
	sm := metrics.NewStorageMetrics(m.Registry())

	// Mount S3 API at root, optionally behind SigV4 middleware
	store := metadata.NewMemoryStore()
	objs, err := storage.NewLocalFS(cfg.DataDirs)
	if err != nil {
		slog.Error("init storage", slog.String("error", err.Error()))
		os.Exit(1)
	}
	// Wire storage metrics observer
	objs.SetObserver(sm)

	api := s3.New(store, objs)

	handler := api.Handler()
	if cfg.AuthMode == "sigv4" {
		// Build static credentials store from config
		keys := make([]sigv4.AccessKey, 0, len(cfg.AccessKeys))
		for _, k := range cfg.AccessKeys {
			keys = append(keys, sigv4.AccessKey{AccessKey: k.AccessKey, SecretKey: k.SecretKey, User: k.User})
		}
		credStore := sigv4.NewStaticStore(keys)
		// Exempt health endpoints from auth
		exempt := func(r *http.Request) bool {
			switch r.URL.Path {
			case "/livez", "/readyz", "/metrics":
				return true
			default:
				return false
			}
		}
		handler = sigv4.Middleware(credStore, exempt)(handler)
		slog.Info("sigv4 auth enabled")
	}
	// Tracing middleware
	handler = tracing.Middleware(handler)
	// Instrument S3 API with HTTP metrics
	handler = m.Middleware(handler)
	mux.Handle("/", handler)

	srv := &http.Server{
		Addr:         cfg.Address,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		ready.Store(true)
		slog.Info("s3free listening", slog.String("version", version), slog.String("addr", cfg.Address))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	// graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ready.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", slog.String("error", err.Error()))
	}
	// Shutdown tracing provider
	if err := traceShutdown(ctx); err != nil {
		slog.Error("tracing shutdown error", slog.String("error", err.Error()))
	}
	slog.Info("s3free stopped")
}
