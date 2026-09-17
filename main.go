package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "run a quick health probe and exit")
	schema := flag.Bool("schema", false, "generate a openapi.json file and exit")

	flag.Parse()

	if *schema {
		if err := writeOpenAPISpecFile("openapi.json"); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}

	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	logger := newLogger(cfg.LogLevel)

	if *healthcheck {
		url, err := healthcheckURL(cfg.ListenAddr)
		if err == nil {
			err = doHealthcheck(url)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}

	if err := run(cfg, logger); err != nil {
		logger.Error("fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run(cfg Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	registry := NewRegistry(cfg.AppIDs())
	state := NewState(cfg.AppIDs(), time.Now())
	cache := NewCache(cfg.CacheDir, cfg.MaxCacheAge)

	elector := Elector(alwaysPrimary{identity: cfg.PodName})

	syncer := NewSyncer(cfg, registry, cache, state, elector, logger)

	// Without a peer service there are no siblings to hydrate from, which
	// is the single-container case.
	var peers *PeerClient
	if cfg.PeerService != "" {
		peers = NewPeerClient(cfg, logger)
		syncer.SetHydrator(peers)
	}

	// Seed from local disk before serving: a restarted container should
	// answer immediately rather than wait on a peer or on upstream.
	syncer.LoadFromCache()

	srv := NewServer(cfg, registry, state, syncer, elector, logger)
	srv.peers = peers

	logger.Info("cacheppuccino starting",
		slog.String("version", version),
		slog.String("pod", cfg.PodName),
		slog.String("addr", cfg.ListenAddr),
		slog.Bool("has_data", registry.Servable()),
		slog.String("election", cfg.LeaderElection),
		slog.String("pull_interval", cfg.PullInterval.String()),
		slog.String("cache_dir", cfg.CacheDir),
		slog.String("peer_service", cfg.PeerService),
	)

	// No BaseContext: request contexts must outlive the shutdown signal
	// so Shutdown can drain in-flight requests instead of aborting them.
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.routes(),
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// The internal listener carries pod-to-pod traffic only. It is never
	// added to the Service or the ingress.
	internalServer := &http.Server{
		Addr:              cfg.InternalListenAddr,
		Handler:           srv.internalRoutes(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serveErr := make(chan error, 2)
	go func() {
		logger.Info("http server starting", slog.String("addr", cfg.ListenAddr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	go func() {
		logger.Info("internal server starting", slog.String("addr", cfg.InternalListenAddr))
		if err := internalServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	syncer.Run(ctx)

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server failed: %w", err)
	case <-ctx.Done():
	}
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown failed", slog.String("err", err.Error()))
	} else {
		logger.Info("http server stopped")
	}
	if err := internalServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("internal shutdown failed", slog.String("err", err.Error()))
	}
	return nil
}

func writeOpenAPISpecFile(path string) error {
	spec, err := buildOpenAPISpec("/")
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o644)
}

func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel})
	return slog.New(h)
}

// healthcheckURL derives the probe URL from LISTEN_ADDR. The probe runs
// inside the same container as the server (the distroless image has no
// curl, so the binary probes itself): wildcard binds (":8080",
// "0.0.0.0:8080", "[::]:8080") are reachable via loopback, while an
// explicit bind host must be probed directly.
func healthcheckURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("invalid LISTEN_ADDR %q: %w", listenAddr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func doHealthcheck(url string) error {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if !isHTTPSuccess(resp.StatusCode) {
		return fmt.Errorf("healthcheck failed: %s", resp.Status)
	}
	return nil
}
