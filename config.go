package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"time"
)

// Translation source kinds: the real IFRC API, or a plain XLSX URL used
// to mock the service on QA/alpha instances.
const (
	sourceAPI = "api"
	sourceURL = "url"
)

type Config struct {
	ListenAddr               string
	SQLitePath               string
	TranslationSource        string
	TranslationBaseURL       string
	TranslationApplicationID string
	TranslationAPIKey        string
	TranslationXLSXURL       string
	HTTPTimeout              time.Duration
	PullInterval             time.Duration
	InitialPullDeadline      time.Duration
	LogLevel                 string
}

var validLogLevels = []string{"debug", "info", "warn", "error"}

// LoadConfig reads configuration from the environment.
// It collects every problem instead of stopping at the first one,
// so a misconfigured deployment reports all mistakes at once.
func LoadConfig() (Config, error) {
	var errs []error

	envDuration := func(k string, def time.Duration) time.Duration {
		v := os.Getenv(k)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid duration for %s: %q", k, v))
			return def
		}
		return d
	}

	cfg := Config{
		ListenAddr:               envOr("LISTEN_ADDR", ":8080"),
		SQLitePath:               envOr("SQLITE_PATH", "/data/cacheppuccino.db"),
		TranslationSource:        envOr("TRANSLATION_SOURCE", sourceAPI),
		TranslationBaseURL:       os.Getenv("TRANSLATION_BASE_URL"),
		TranslationApplicationID: os.Getenv("TRANSLATION_APPLICATION_ID"),
		TranslationAPIKey:        os.Getenv("TRANSLATION_API_KEY"),
		TranslationXLSXURL:       os.Getenv("TRANSLATION_XLSX_URL"),
		HTTPTimeout:              envDuration("HTTP_TIMEOUT", 30*time.Second),
		PullInterval:             envDuration("PULL_INTERVAL", 10*time.Minute),
		InitialPullDeadline:      envDuration("INITIAL_PULL_DEADLINE", 45*time.Second),
		LogLevel:                 envOr("LOG_LEVEL", "info"),
	}

	switch cfg.TranslationSource {
	case sourceAPI:
		for _, k := range []string{"TRANSLATION_BASE_URL", "TRANSLATION_APPLICATION_ID", "TRANSLATION_API_KEY"} {
			if os.Getenv(k) == "" {
				errs = append(errs, fmt.Errorf("missing required env: %s (required when TRANSLATION_SOURCE=api)", k))
			}
		}
	case sourceURL:
		if cfg.TranslationXLSXURL == "" {
			errs = append(errs, errors.New("missing required env: TRANSLATION_XLSX_URL (required when TRANSLATION_SOURCE=url)"))
		} else if u, err := url.Parse(cfg.TranslationXLSXURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("invalid TRANSLATION_XLSX_URL: %q (must be an http(s) URL)", cfg.TranslationXLSXURL))
		}
	default:
		errs = append(errs, fmt.Errorf("invalid TRANSLATION_SOURCE: %q (valid: api, url)", cfg.TranslationSource))
	}

	if cfg.HTTPTimeout <= 0 {
		errs = append(errs, fmt.Errorf("HTTP_TIMEOUT must be positive, got %s", cfg.HTTPTimeout))
	}
	if cfg.PullInterval <= 0 {
		errs = append(errs, fmt.Errorf("PULL_INTERVAL must be positive, got %s", cfg.PullInterval))
	}
	// Zero means "no deadline" for the initial pull; only negatives are invalid.
	if cfg.InitialPullDeadline < 0 {
		errs = append(errs, fmt.Errorf("INITIAL_PULL_DEADLINE must not be negative, got %s", cfg.InitialPullDeadline))
	}

	if !slices.Contains(validLogLevels, cfg.LogLevel) {
		errs = append(errs, fmt.Errorf("invalid LOG_LEVEL: %q (valid: debug, info, warn, error)", cfg.LogLevel))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

func envOr(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}
