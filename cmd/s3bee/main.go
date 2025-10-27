package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"s3bee/pkg/config"
	"s3bee/pkg/api/s3"
	"s3bee/pkg/metadata"
	"s3bee/pkg/storage"
)

var version = "0.0.1-dev"

func main() {
	// Load config from S3BEE_CONFIG or ./config.yaml; defaults otherwise.
	cfgPath := os.Getenv("S3BEE_CONFIG")
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

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); _, _ = w.Write([]byte("ready")) })

	// Mount initial S3 API (stubbed) at root
	store := metadata.NewMemoryStore()
	objs, err := storage.NewLocalFS(cfg.DataDirs)
	if err != nil { slog.Error("init storage", slog.String("error", err.Error())); os.Exit(1) }
	api := s3.New(store, objs)
	mux.Handle("/", api.Handler())

	srv := &http.Server{
		Addr:         cfg.Address,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("s3bee listening", slog.String("version", version), slog.String("addr", cfg.Address))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	// graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", slog.String("error", err.Error()))
	}
	slog.Info("s3bee stopped")
}
