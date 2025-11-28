package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	adminpkg "shardseal/pkg/admin"
	"shardseal/pkg/api/s3"
	"shardseal/pkg/config"
	"shardseal/pkg/metadata"
	"shardseal/pkg/obs/metrics"
	"shardseal/pkg/obs/tracing"
	"shardseal/pkg/repair"
	adminoidc "shardseal/pkg/security/oidc"
	"shardseal/pkg/security/sigv4"
	"shardseal/pkg/storage"
)

var version = "0.0.1-dev"
var ready atomic.Bool

func main() {
	// Load config from SHARDSEAL_CONFIG or ./config.yaml; defaults otherwise.
	cfgPath := os.Getenv("SHARDSEAL_CONFIG")
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
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
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

	// Sealed I/O toggle (experimental): wire storage behavior
	objs.SetSealed(cfg.Sealed.Enabled, cfg.Sealed.VerifyOnRead)
	slog.Info("sealed I/O",
		slog.Bool("enabled", cfg.Sealed.Enabled),
		slog.Bool("verifyOnRead", cfg.Sealed.VerifyOnRead),
	)

	api := s3.NewWithLimits(store, objs, s3.Limits{
		SinglePutMaxBytes:    cfg.Limits.SinglePutMaxBytes,
		MinMultipartPartSize: cfg.Limits.MinMultipartPartSize,
	})

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
	handler = tracing.Middleware(handler, cfg.Tracing.KeyHashEnabled)
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

	// Optional Admin server on separate port with read-only endpoints
	var adminSrv *http.Server
	var gcStop context.CancelFunc
	var scrub repair.Scrubber
	var scrubPollStop func()
	var repQ repair.RepairQueue
	var repQPollStop func()
	var repW *repair.Worker

	// Initialize repair queue/worker when enabled via config (always-on, independent of Admin API)
	if cfg.Repair.Enabled {
		repQ = repair.NewMemQueue(1024)
		rm := metrics.NewRepairMetrics(m.Registry())
		// Wire storage repair callback to enqueue into repair queue
		objs.SetRepairCallback(func(ctx context.Context, it storage.RepairItem) error {
			rit := repair.RepairItem{
				Bucket:     it.Bucket,
				Key:        it.Key,
				ShardPath:  it.ShardPath,
				Reason:     it.Reason,
				Priority:   repair.PriorityForReason(it.Reason),
				Discovered: it.Discovered,
			}
			rm.ObserveEnqueue(rit.Reason)
			return repQ.Enqueue(ctx, rit)
		})
		// Start worker and metrics if enabled
		repQPollStop = rm.StartPolling(repQ, 10*time.Second)
		if cfg.Repair.WorkerEnabled {
			conc := cfg.Repair.WorkerConcurrency
			if conc <= 0 {
				conc = 1
			}
			repW = repair.NewWorker(repQ, repair.WorkerConfig{Concurrency: conc})
			rewriter := repair.NewSingleShardRewriter(objs.BaseDir())
			repW.SetProcessor(func(ctx context.Context, it repair.RepairItem) error {
				start := time.Now()
				err := rewriter.Repair(ctx, it)
				result := "success"
				switch {
				case errors.Is(err, repair.ErrSkip), errors.Is(err, repair.ErrUnsupported):
					result = "skipped"
					err = nil
				case err != nil:
					result = "failed"
				}
				rm.ObserveResult(result, time.Since(start))
				return err
			})
			_ = repW.Start(context.Background())
		}
	}

	// Start scrubber based on config, independent of Admin API. Admin may still expose controls.
	if cfg.Scrubber.Enabled {
		interval, ierr := time.ParseDuration(cfg.Scrubber.Interval)
		if ierr != nil || interval <= 0 {
			interval = time.Hour
		}
		concurrency := cfg.Scrubber.Concurrency
		if concurrency <= 0 {
			concurrency = 1
		}
		// Determine scrubber payload verification: explicit config overrides sealed.verifyOnRead
		verifyPayload := cfg.Sealed.VerifyOnRead
		if cfg.Scrubber.VerifyPayload != nil {
			verifyPayload = *cfg.Scrubber.VerifyPayload
		}
		ss := repair.NewSealedScrubber(cfg.DataDirs, repair.Config{
			Interval:      interval,
			Concurrency:   concurrency,
			VerifyPayload: verifyPayload,
		})
		// If repair queue exists, wire it; otherwise scrubber still verifies and updates metrics
		if repQ != nil {
			ss.SetRepairQueue(repQ)
		}
		_ = ss.Start(context.Background())
		scrub = ss
		smScr := metrics.NewScrubberMetrics(m.Registry())
		scrubPollStop = smScr.StartPolling(scrub, 10*time.Second)
	}
	if cfg.AdminAddress != "" {
		adminMux := http.NewServeMux()
		// /admin/health: reports liveness and readiness along with version and listen address
		adminMux.HandleFunc("/admin/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"status":    "ok",
				"ready":     ready.Load(),
				"version":   version,
				"address":   cfg.Address,
				"admin":     cfg.AdminAddress,
				"timestamp": time.Now().UTC().Format(time.RFC3339),
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
		// /admin/version: returns version info
		adminMux.HandleFunc("/admin/version", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]string{
				"version":   version,
				"timestamp": time.Now().UTC().Format(time.RFC3339),
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
		// POST /admin/gc/multipart?olderThan=24h
		// Use shared admin package handler to run a single GC pass (best-effort).
		adminMux.Handle("/admin/gc/multipart", adminpkg.NewMultipartGCHandler(store, objs))

		// Scrubber admin endpoints (experimental): GET /admin/scrub/stats, POST /admin/scrub/runonce
		// Create a sealed scrubber when enabled by config; verifies header/footer CRC and content hash.
		// Payload re-hash verification is tied to sealed.verifyOnRead for now.
		// If scrubber is running from config, expose admin controls; otherwise handlers still work (return empty/defaults)
		adminMux.Handle("/admin/scrub/stats", adminpkg.NewScrubberStatsHandler(scrub))
		adminMux.Handle("/admin/scrub/runonce", adminpkg.NewScrubberRunOnceHandler(scrub))

		// Repair queue admin endpoints (queue may be nil if not enabled)
		adminMux.Handle("/admin/repair/stats", adminpkg.NewRepairQueueStatsHandler(repQ))
		adminMux.Handle("/admin/repair/enqueue", adminpkg.NewRepairEnqueueHandler(repQ))
		// Worker controls (no-op if worker not started)
		adminMux.Handle("/admin/repair/worker/stats", adminpkg.NewRepairWorkerStatsHandler(repW))
		adminMux.Handle("/admin/repair/worker/pause", adminpkg.NewRepairWorkerPauseHandler(repW))
		adminMux.Handle("/admin/repair/worker/resume", adminpkg.NewRepairWorkerResumeHandler(repW))

		// Optionally protect Admin API with OIDC when enabled
		adminHandler := http.Handler(adminMux)
		if cfg.OIDC.Enabled {
			v, err := adminoidc.NewVerifier(context.Background(), adminoidc.Config{
				Issuer:   cfg.OIDC.Issuer,
				ClientID: cfg.OIDC.ClientID,
				Audience: cfg.OIDC.Audience,
				JWKSURL:  cfg.OIDC.JWKSURL,
			})
			if err != nil {
				slog.Error("admin oidc init failed", slog.String("error", err.Error()))
			} else {
				// Exemptions for health/version if allowed by config (useful for LB/K8s probes)
				exempt := func(r *http.Request) bool {
					if cfg.OIDC.AllowUnauthHealth && r.URL.Path == "/admin/health" {
						return true
					}
					if cfg.OIDC.AllowUnauthVersion && r.URL.Path == "/admin/version" {
						return true
					}
					return false
				}
				// Compose middleware so that OIDC runs before RBAC (ensuring subject is present for RBAC).
				adminHandler = adminoidc.RBAC(adminoidc.DefaultAdminPolicy())(adminHandler)
				adminHandler = adminoidc.Middleware(v, exempt)(adminHandler)
				slog.Info("admin oidc enabled",
					slog.Bool("allowUnauthHealth", cfg.OIDC.AllowUnauthHealth),
					slog.Bool("allowUnauthVersion", cfg.OIDC.AllowUnauthVersion),
				)
			}
		}

		adminSrv = &http.Server{
			Addr:         cfg.AdminAddress,
			Handler:      adminHandler,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  30 * time.Second,
		}
		go func() {
			slog.Info("admin listening", slog.String("addr", cfg.AdminAddress))
			if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("admin server error", slog.String("error", err.Error()))
				os.Exit(1)
			}
		}()
	}

	// Background multipart GC (best-effort), controlled by config.GC
	if cfg.GC.Enabled {
		interval, ierr := time.ParseDuration(cfg.GC.Interval)
		if ierr != nil || interval <= 0 {
			interval = 15 * time.Minute
		}
		olderThan, oerr := time.ParseDuration(cfg.GC.OlderThan)
		if oerr != nil || olderThan <= 0 {
			olderThan = 24 * time.Hour
		}
		var gcCtx context.Context
		gcCtx, gcStop = context.WithCancel(context.Background())
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			const stagingBucket = ".multipart"
			for {
				select {
				case <-gcCtx.Done():
					return
				case <-ticker.C:
					res, err := adminpkg.RunMultipartGC(context.Background(), store, objs, olderThan)
					if err != nil {
						slog.Error("gc: run", slog.String("error", err.Error()))
						continue
					}
					slog.Info("gc: multipart pass",
						slog.Int("scanned", res.Scanned),
						slog.Int("aborted", res.Aborted),
						slog.Int("deleted", res.Deleted),
					)
				}
			}
		}()
		slog.Info("multipart GC enabled", slog.String("interval", interval.String()), slog.String("olderThan", olderThan.String()))
	}

	go func() {
		ready.Store(true)
		slog.Info("shardseal listening", slog.String("version", version), slog.String("addr", cfg.Address))
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
	// Shutdown admin server if running
	if adminSrv != nil {
		if err := adminSrv.Shutdown(ctx); err != nil {
			slog.Error("admin shutdown error", slog.String("error", err.Error()))
		}
	}
	// Stop background GC if running
	if gcStop != nil {
		gcStop()
	}
	// Stop scrubber if running
	if scrub != nil {
		_ = scrub.Stop(ctx)
	}
	// Stop scrubber metrics poller
	if scrubPollStop != nil {
		scrubPollStop()
	}
	// Stop repair metrics poller
	if repQPollStop != nil {
		repQPollStop()
	}
	// Stop repair worker
	if repW != nil {
		_ = repW.Stop(ctx)
	}
	// Close repair queue
	if repQ != nil {
		_ = repQ.Close()
	}
	// Shutdown tracing provider
	if err := traceShutdown(ctx); err != nil {
		slog.Error("tracing shutdown error", slog.String("error", err.Error()))
	}
	slog.Info("shardseal stopped")
}
