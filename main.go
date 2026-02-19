package main

import (
	"context"
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

func main() {
	healthcheck := flag.Bool("healthcheck", false, "run a quick health probe and exit")
	flag.Parse()

	cfg := LoadConfig()
	logger := newLogger(cfg.LogLevel)

	if *healthcheck {
		err := doHealthcheck("http://127.0.0.1" + cfg.ListenAddr + "/healthz")
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

	ready := &ReadyState{}
	ready.SetReady(true)

	srv := &Server{db: db, ready: ready, logger: logger}

	logger.Info("cacheppuccino starting",
		slog.String("addr", cfg.ListenAddr),
		slog.String("pull_interval", cfg.PullInterval.String()),
	)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.routes(),
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext: func(l net.Listener) context.Context {
			return ctx
		},
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
