// Command branchdam runs the branchDAM server: it indexes configured storage
// locations into a version node graph, resolves lineage edges between them,
// and serves the SPA + REST/SSE API.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/httpapi"
)

// version is stamped at build time via -ldflags "-X main.version=...".
// Defaults to "dev" for local builds (see Dockerfile, added in PR 11).
var version = "dev"

func main() {
	cfgPath := flag.String("config", envOr("BRANCHDAM_CONFIG", "config.yaml"), "path to config file")
	debug := flag.Bool("debug", os.Getenv("BRANCHDAM_DEBUG") != "", "enable debug logging")
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit (for container HEALTHCHECK)")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}

	if *healthcheck {
		os.Exit(runHealthcheck(cfg.ListenAddr))
	}

	log.Info("loaded config", "version", version, "listenAddr", cfg.ListenAddr)

	srv := httpapi.New(&cfg, log)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Duration(cfg.HTTP.ReadTimeoutSecs) * time.Second,
		WriteTimeout:      time.Duration(cfg.HTTP.WriteTimeoutSecs) * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown error", "err", err)
		os.Exit(1)
	}
	log.Info("server stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
