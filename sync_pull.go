package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// TranslationClient fetches the XLSX export from the IFRC translation API.
type TranslationClient struct {
	baseURL       string
	applicationID string
	apiKey        string
	http          *http.Client
}

func NewTranslationClient(cfg Config) *TranslationClient {
	return &TranslationClient{
		baseURL:       cfg.TranslationBaseURL,
		applicationID: cfg.TranslationApplicationID,
		apiKey:        cfg.TranslationAPIKey,
		http: &http.Client{
			Timeout: cfg.HTTPTimeout,
		},
	}
}

func (c *TranslationClient) Name() string { return "api" }

func (c *TranslationClient) Fetch(ctx context.Context, logger *slog.Logger) ([]byte, error) {
	url := fmt.Sprintf("%s/api/Application/%s/Translation/export", c.baseURL, c.applicationID)
	logger.Info("pull: requesting export", slog.String("url", url))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	if c.apiKey != "" {
		req.Header.Set("X-API-KEY", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("export request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("download failed: %s: %s", resp.Status, string(b))
	}

	return readAllLimited(resp.Body, maxXLSXBytes)
}

type PullResult struct {
	Skipped bool
	Hash    string
	Rows    int
}

func PullOnce(
	ctx context.Context,
	db *DB,
	source XLSXSource,
	logger *slog.Logger,
) (PullResult, error) {
	t0 := time.Now()

	xlsx, err := source.Fetch(ctx, logger)
	if err != nil {
		return PullResult{}, err
	}
	logger.Info("pull: download done", slog.Duration("dur", time.Since(t0)), slog.Int("bytes", len(xlsx)))

	hash := HashBytes(xlsx)

	prev, ok, err := db.GetMeta(ctx, metaKeyLastHash)
	if err != nil {
		return PullResult{}, err
	}

	if ok && prev == hash {
		// Content unchanged; still record that a pull succeeded.
		if err := db.SetMeta(ctx, metaKeyLastPull, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return PullResult{}, err
		}
		return PullResult{Skipped: true, Hash: hash, Rows: 0}, nil
	}

	t1 := time.Now()
	rows, err := ParseXLSX(xlsx)
	if err != nil {
		return PullResult{}, err
	}
	logger.Info("pull: parse done", slog.Duration("dur", time.Since(t1)), slog.Int("rows", len(rows)))

	t2 := time.Now()
	if err := db.ReplaceImport(ctx, rows, hash, time.Now()); err != nil {
		return PullResult{}, err
	}
	logger.Info("pull: import done", slog.Duration("dur", time.Since(t2)))

	return PullResult{Skipped: false, Hash: hash, Rows: len(rows)}, nil
}

func StartPeriodicPuller(
	ctx context.Context,
	db *DB,
	source XLSXSource,
	interval time.Duration,
	initialDeadline time.Duration,
	ready *ReadyState,
	logger *slog.Logger,
) {
	jitterMax := time.Duration(float64(interval) * 0.10)
	ticker := time.NewTicker(interval)

	// runPull records the outcome in meta so /status can report the most
	// recent pull error without access to pod logs (QA has none).
	runPull := func(pullCtx context.Context, kind string) {
		res, err := PullOnce(pullCtx, db, source, logger)
		if err != nil {
			// Skip recording on shutdown; the parent ctx is the process ctx.
			if ctx.Err() == nil {
				if merr := db.SetMeta(ctx, metaKeyLastPullError, err.Error()); merr != nil {
					logger.Warn("pull: failed to record pull error", "err", merr)
				}
			}
			logger.Warn(kind+" pull failed; serving cached data if any", "err", err)
			return
		}

		if merr := db.SetMeta(ctx, metaKeyLastPullError, ""); merr != nil {
			logger.Warn("pull: failed to clear pull error", "err", merr)
		}
		ready.SetReady(true)

		if res.Skipped {
			logger.Info(kind+" pull unchanged", slog.String("hash", res.Hash))
		} else {
			logger.Info(kind+" pull imported", slog.Int("rows", res.Rows), slog.String("hash", res.Hash))
		}
	}

	go func() {
		defer ticker.Stop()

		// Immediate pull on startup (async; does not block HTTP server startup)
		func() {
			logger.Info("initial pull started", slog.String("source", source.Name()))

			pullCtx := ctx
			cancel := func() {}
			if initialDeadline > 0 {
				pullCtx, cancel = context.WithTimeout(ctx, initialDeadline)
			}
			defer cancel()

			runPull(pullCtx, "initial")
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if jitterMax > 0 {
					sleep := time.Duration(time.Now().UnixNano() % int64(jitterMax+1))
					timer := time.NewTimer(sleep)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}

				runPull(ctx, "periodic")
			}
		}
	}()
}
