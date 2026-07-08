package main

import (
	"context"
	"encoding/json"
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
		if err := writeSchemaFile("openapi.json"); err != nil {
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

	db, err := OpenDB(cfg.SQLitePath)
	if err != nil {
		logger.Error("db open failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	client := NewTranslationClient(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Ready = can serve: data from a previous run counts, even if the
	// upstream is currently unreachable. A pull success also sets it.
	ready := &ReadyState{}
	hasData, err := db.HasStrings(ctx)
	if err != nil {
		logger.Error("readiness check failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
	ready.SetReady(hasData)

	srv := &Server{db: db, ready: ready, logger: logger}

	logger.Info("cacheppuccino starting",
		slog.String("version", version),
		slog.String("addr", cfg.ListenAddr),
		slog.Bool("has_data", hasData),
		slog.String("pull_interval", cfg.PullInterval.String()),
		slog.String("http_timeout", cfg.HTTPTimeout.String()),
		slog.String("initial_pull_deadline", cfg.InitialPullDeadline.String()),
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

	go func() {
		logger.Info("http server starting", slog.String("addr", cfg.ListenAddr))
		err := httpServer.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			logger.Error("http server failed", slog.String("err", err.Error()))
			stop()
		}
	}()

	StartPeriodicPuller(
		ctx,
		db,
		client,
		cfg.PullInterval,
		cfg.InitialPullDeadline,
		cfg.TranslationApplicationID,
		ready,
		logger,
	)

	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown failed", slog.String("err", err.Error()))
	} else {
		logger.Info("http server stopped")
	}
}

func writeSchemaFile(path string) error {
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
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

// healthcheckURL derives the local probe URL from LISTEN_ADDR,
// which may be ":8080", "0.0.0.0:8080", or "host:8080".
func healthcheckURL(listenAddr string) (string, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("invalid LISTEN_ADDR %q: %w", listenAddr, err)
	}
	return "http://127.0.0.1:" + port + "/healthz", nil
}

func doHealthcheck(url string) error {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("healthcheck failed: %s", resp.Status)
	}
	return nil
}
