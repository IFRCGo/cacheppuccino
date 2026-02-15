package main

import (
	"log"
	"os"
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

func LoadConfig() Config {
	return Config{
		ListenAddr:               env("LISTEN_ADDR", ":8080"),
		SQLitePath:               env("SQLITE_PATH", "/data/cacheppuccino.db"),
		TranslationBaseURL:       mustEnv("TRANSLATION_BASE_URL"),
		TranslationApplicationID: mustEnv("TRANSLATION_APPLICATION_ID"),
		TranslationAPIKey:        mustEnv("TRANSLATION_API_KEY"),
		HTTPTimeout:              envDuration("HTTP_TIMEOUT", 30*time.Second),
		PullInterval:             envDuration("PULL_INTERVAL", 10*time.Minute),
		InitialPullDeadline:      envDuration("INITIAL_PULL_DEADLINE", 45*time.Second),
		LogLevel:                 env("LOG_LEVEL", "info"),
	}
}

func env(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing required env: %s", k)
	}
	return v
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
