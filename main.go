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

	elector, electionRunner, err := newElector(cfg, logger)
	if err != nil {
		return err
	}

	syncer := NewSyncer(cfg, registry, cache, state, elector, logger)

	// Without a peer service there are no siblings to hydrate from, which
	// is the single-container case.
	var peers *PeerClient
	if cfg.PeerService != "" {
		peers = NewPeerClient(cfg, logger)
		syncer.SetHydrator(peers)
	}

	srv := NewServer(cfg, registry, state, syncer, elector, logger)
	srv.peers = peers

	logger.Info("cacheppuccino starting",
		slog.String("version", version),
		slog.String("pod", cfg.PodName),
		slog.String("addr", cfg.ListenAddr),
		slog.String("election", cfg.LeaderElection),
		slog.String("pull_interval", cfg.PullInterval.String()),
		slog.String("cache_dir", cfg.CacheDir),
		slog.String("peer_service", cfg.PeerService),
	)

	// The public listener carries client traffic. The internal one carries
	// pod-to-pod traffic only and is never added to the Service or the
	// ingress.
	httpServer := newHTTPServer(cfg.ListenAddr, srv.routes())
	httpServer.ReadTimeout = 10 * time.Second
	httpServer.WriteTimeout = 15 * time.Second
	internalServer := newHTTPServer(cfg.InternalListenAddr, srv.internalRoutes())

	// Ordered: the public listener drains first on shutdown, so client
	// traffic stops before peers lose the endpoints they hydrate from.
	servers := []namedServer{
		{name: "public", server: httpServer},
		{name: "internal", server: internalServer},
	}
	serveErr := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			logger.Info("http server starting", slog.String("listener", s.name), slog.String("addr", s.server.Addr))
			if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("%s listener: %w", s.name, err)
			}
		}()
	}

	// Seeding from disk after the listeners bind keeps probes answerable
	// while the cached export is parsed; /readyz reports not-ready until it
	// lands either way.
	syncer.LoadFromCache()

	electionDone := make(chan struct{})
	if electionRunner == nil {
		close(electionDone)
	} else {
		go func() {
			defer close(electionDone)
			electionRunner(ctx)
		}()
	}
	syncer.Run(ctx)

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server failed: %w", err)
	case <-ctx.Done():
	}
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, s := range servers {
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			logger.Error("http shutdown failed", slog.String("listener", s.name), slog.String("err", err.Error()))
		}
	}
	logger.Info("http servers stopped")

	// The background loops outlive the listeners: the elector still has to
	// hand the lease back, and a pull may be mid-write to the cache. Without
	// this the process exits first and a successor waits out the full lease
	// duration.
	loopsDone := make(chan struct{})
	go func() {
		defer close(loopsDone)
		<-electionDone
		syncer.Wait()
	}()
	select {
	case <-loopsDone:
	case <-shutdownCtx.Done():
		logger.Warn("background loops did not stop within the shutdown budget")
	}
	return nil
}

// namedServer pairs a listener with the name its log lines carry.
type namedServer struct {
	name   string
	server *http.Server
}

// newHTTPServer builds a listener with the timeouts both servers share.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	// No BaseContext: request contexts must outlive the shutdown signal so
	// Shutdown can drain in-flight requests instead of aborting them.
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// newElector returns the elector and, when election is active, the loop that
// maintains it. A single container has nobody to coordinate with, so it is
// unconditionally primary.
func newElector(cfg Config, logger *slog.Logger) (Elector, func(context.Context), error) {
	if cfg.LeaderElection != electionLease {
		return alwaysPrimary{identity: cfg.PodName}, nil, nil
	}

	client, err := NewLeaseClient(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("leader election: %w", err)
	}
	e := NewLeaseElector(client, cfg, logger)
	return e, e.Run, nil
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
