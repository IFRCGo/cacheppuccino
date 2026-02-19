package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

type TranslationClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewTranslationClient(cfg Config) *TranslationClient {
	return &TranslationClient{
		baseURL: cfg.TranslationBaseURL,
		apiKey:  cfg.TranslationAPIKey,
		http: &http.Client{
			Timeout: cfg.HTTPTimeout,
		},
	}
}

func (c *TranslationClient) DownloadXLSX(ctx context.Context, applicationID string, logger *slog.Logger) ([]byte, error) {
	url := fmt.Sprintf("%s/api/Application/%s/Translation/export", c.baseURL, applicationID)

	logger.Info("Requesting export", slog.String("url", url))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	if c.apiKey != "" {
		req.Header.Set("X-API-KEY", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("download failed: %s: %s", resp.Status, string(b))
	}

	return io.ReadAll(resp.Body)
}

type PullResult struct {
	Skipped bool
	Hash    string
	Rows    int
}

func PullOnce(
	ctx context.Context,
	db *DB,
	client *TranslationClient,
	applicationID string,
	logger *slog.Logger,
) (PullResult, error) {
	xlsx, err := client.DownloadXLSX(ctx, applicationID, logger)
	if err != nil {
		return PullResult{}, err
	}

	hash := HashBytes(xlsx)

	prev, ok, err := db.GetMeta(ctx, "last_xlsx_sha256")
	if err != nil {
		return PullResult{}, err
	}

	if ok && prev == hash {
		return PullResult{Skipped: true, Hash: hash, Rows: 0}, nil
	}

	rows, err := ParseXLSX(xlsx)
	if err != nil {
		return PullResult{}, err
	}

	for _, r := range rows {
		err := db.UpsertString(ctx, r.Page, r.Key, r.Lang, r.Value, r.UpdatedAt)
		if err != nil {
			return PullResult{}, err
		}
	}

	err = db.SetMeta(ctx, "last_xlsx_sha256", hash)
	if err != nil {
		return PullResult{}, err
	}

	err = db.SetMeta(ctx, "last_pull_rfc3339", time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return PullResult{}, err
	}

	return PullResult{Skipped: false, Hash: hash, Rows: len(rows)}, nil
}

func StartPeriodicPuller(
	ctx context.Context,
	db *DB,
	client *TranslationClient,
	interval time.Duration,
	initialDeadline time.Duration,
	applicationID string,
	ready *ReadyState,
	logger *slog.Logger,
) {
	jitterMax := time.Duration(float64(interval) * 0.10)
	ticker := time.NewTicker(interval)

	go func() {
		defer ticker.Stop()

		// Immediate pull on startup (async; does not block HTTP server startup)
		func() {
			logger.Info("initial pull started")

			pullCtx := ctx
			cancel := func() {}
			if initialDeadline > 0 {
				pullCtx, cancel = context.WithTimeout(ctx, initialDeadline)
			}
			defer cancel()

			res, err := PullOnce(pullCtx, db, client, applicationID, logger)
			if err != nil {
				if logger != nil {
					logger.Warn("initial pull failed; service remains unready until a pull succeeds", "err", err)
				}
				return
			}

			if ready != nil {
				ready.SetReady(true)
			}

			if logger != nil {
				if res.Skipped {
					logger.Info("initial pull unchanged", slog.String("hash", res.Hash))
				} else {
					logger.Info("initial pull imported", slog.Int("rows", res.Rows), slog.String("hash", res.Hash))
				}
			}
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

				res, err := PullOnce(ctx, db, client, applicationID, logger)
				if err != nil {
					if logger != nil {
						logger.Warn("periodic pull failed", "err", err)
					}
					continue
				}

				if ready != nil {
					ready.SetReady(true)
				}

				if logger != nil {
					if res.Skipped {
						logger.Info("periodic pull unchanged", slog.String("hash", res.Hash))
					} else {
						logger.Info("periodic pull imported", slog.Int("rows", res.Rows), slog.String("hash", res.Hash))
					}
				}
			}
		}
	}()
}
