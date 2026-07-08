package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"time"
)

type Config struct {
	ListenAddr               string
	SQLitePath               string
	TranslationBaseURL       string
	TranslationApplicationID string
	TranslationAPIKey        string
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

	requireEnv := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			errs = append(errs, fmt.Errorf("missing required env: %s", k))
		}
		return v
	}

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
		ListenAddr:               env("LISTEN_ADDR", ":8080"),
		SQLitePath:               env("SQLITE_PATH", "/data/cacheppuccino.db"),
		TranslationBaseURL:       requireEnv("TRANSLATION_BASE_URL"),
		TranslationApplicationID: requireEnv("TRANSLATION_APPLICATION_ID"),
		TranslationAPIKey:        requireEnv("TRANSLATION_API_KEY"),
		HTTPTimeout:              envDuration("HTTP_TIMEOUT", 30*time.Second),
		PullInterval:             envDuration("PULL_INTERVAL", 10*time.Minute),
		InitialPullDeadline:      envDuration("INITIAL_PULL_DEADLINE", 45*time.Second),
		LogLevel:                 env("LOG_LEVEL", "info"),
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

func env(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}
