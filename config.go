package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"time"
)

// Translation source kinds: the real IFRC API, or a plain XLSX URL used
// to mock the service on QA/alpha instances.
const (
	sourceAPI = "api"
	sourceURL = "url"
)

// Leader election modes. "lease" elects a single primary through a
// Kubernetes Lease; "off" makes the process unconditionally primary, which
// is what a single container under docker compose needs.
const (
	electionLease = "lease"
	electionOff   = "off"
)

// defaultAppID names the single configured application. Internals are keyed
// by application id so that serving several applications later is a config
// change rather than a rework.
const defaultAppID = "default"

// AppConfig describes one application's translation source.
type AppConfig struct {
	ID            string
	Source        string
	BaseURL       string
	ApplicationID string
	APIKey        string
	XLSXURL       string
	PullInterval  time.Duration
}

type Config struct {
	ListenAddr         string
	InternalListenAddr string
	LogLevel           string

	Apps []AppConfig

	CacheDir    string
	MaxCacheAge time.Duration

	PeerService      string
	PeerPollInterval time.Duration
	PeerTimeout      time.Duration

	LeaderElection     string
	LeaseName          string
	LeaseDuration      time.Duration
	LeaseRenewInterval time.Duration

	PullInterval          time.Duration
	PullConcurrency       int
	HTTPTimeout           time.Duration
	InitialPullDeadline   time.Duration
	InitialPullBackoffMin time.Duration
	InitialPullBackoffMax time.Duration

	AlarmNoPrimary    time.Duration
	AlarmSnapshotAge  time.Duration
	AlarmDivergence   time.Duration
	AlarmPeerUnready  time.Duration
	AlarmPullFailures int

	ClusterCacheTTL time.Duration

	PodName      string
	PodNamespace string
	PodIP        string
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

	envInt := func(k string, def int) int {
		v := os.Getenv(k)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid integer for %s: %q", k, v))
			return def
		}
		return n
	}

	cfg := Config{
		ListenAddr:         envOr("LISTEN_ADDR", ":8080"),
		InternalListenAddr: envOr("INTERNAL_LISTEN_ADDR", ":8081"),
		LogLevel:           envOr("LOG_LEVEL", "info"),

		CacheDir:    envOr("CACHE_DIR", "/cache"),
		MaxCacheAge: envDuration("MAX_CACHE_AGE", 24*time.Hour),

		PeerService:      os.Getenv("PEER_SERVICE"),
		PeerPollInterval: envDuration("PEER_POLL_INTERVAL", 5*time.Second),
		PeerTimeout:      envDuration("PEER_TIMEOUT", 3*time.Second),

		LeaderElection:     envOr("LEADER_ELECTION", electionOff),
		LeaseName:          envOr("LEASE_NAME", "cacheppuccino"),
		LeaseDuration:      envDuration("LEASE_DURATION", 15*time.Second),
		LeaseRenewInterval: envDuration("LEASE_RENEW_INTERVAL", 5*time.Second),

		PullInterval:          envDuration("PULL_INTERVAL", 10*time.Minute),
		PullConcurrency:       envInt("PULL_CONCURRENCY", 4),
		HTTPTimeout:           envDuration("HTTP_TIMEOUT", 30*time.Second),
		InitialPullDeadline:   envDuration("INITIAL_PULL_DEADLINE", 45*time.Second),
		InitialPullBackoffMin: envDuration("INITIAL_PULL_BACKOFF_MIN", 2*time.Second),
		InitialPullBackoffMax: envDuration("INITIAL_PULL_BACKOFF_MAX", 30*time.Second),

		AlarmNoPrimary:    envDuration("ALARM_NO_PRIMARY", 5*time.Minute),
		AlarmSnapshotAge:  envDuration("ALARM_SNAPSHOT_AGE", 0),
		AlarmDivergence:   envDuration("ALARM_DIVERGENCE", 2*time.Minute),
		AlarmPeerUnready:  envDuration("ALARM_PEER_UNREADY", 2*time.Minute),
		AlarmPullFailures: envInt("ALARM_PULL_FAILURES", 3),

		ClusterCacheTTL: envDuration("CLUSTER_CACHE_TTL", 5*time.Second),

		PodName:      envOr("POD_NAME", hostnameOr("cacheppuccino")),
		PodNamespace: envOr("POD_NAMESPACE", "default"),
		PodIP:        os.Getenv("POD_IP"),
	}

	// Zero means "derive from the pull interval": an alarm threshold that
	// silently ignores a shortened PULL_INTERVAL would never fire in time.
	if cfg.AlarmSnapshotAge == 0 {
		cfg.AlarmSnapshotAge = 4 * cfg.PullInterval
	}

	app := AppConfig{
		ID:            defaultAppID,
		Source:        envOr("TRANSLATION_SOURCE", sourceAPI),
		BaseURL:       os.Getenv("TRANSLATION_BASE_URL"),
		ApplicationID: os.Getenv("TRANSLATION_APPLICATION_ID"),
		APIKey:        os.Getenv("TRANSLATION_API_KEY"),
		XLSXURL:       os.Getenv("TRANSLATION_XLSX_URL"),
		PullInterval:  cfg.PullInterval,
	}

	switch app.Source {
	case sourceAPI:
		for _, k := range []string{"TRANSLATION_BASE_URL", "TRANSLATION_APPLICATION_ID", "TRANSLATION_API_KEY"} {
			if os.Getenv(k) == "" {
				errs = append(errs, fmt.Errorf("missing required env: %s (required when TRANSLATION_SOURCE=api)", k))
			}
		}
	case sourceURL:
		if app.XLSXURL == "" {
			errs = append(errs, errors.New("missing required env: TRANSLATION_XLSX_URL (required when TRANSLATION_SOURCE=url)"))
		} else if u, err := url.Parse(app.XLSXURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("invalid TRANSLATION_XLSX_URL: %q (must be an http(s) URL)", app.XLSXURL))
		}
	default:
		errs = append(errs, fmt.Errorf("invalid TRANSLATION_SOURCE: %q (valid: api, url)", app.Source))
	}

	if err := validateAppID(app.ID); err != nil {
		errs = append(errs, err)
	}
	cfg.Apps = []AppConfig{app}

	if cfg.HTTPTimeout <= 0 {
		errs = append(errs, fmt.Errorf("HTTP_TIMEOUT must be positive, got %s", cfg.HTTPTimeout))
	}
	if cfg.PullInterval <= 0 {
		errs = append(errs, fmt.Errorf("PULL_INTERVAL must be positive, got %s", cfg.PullInterval))
	}
	if cfg.PullConcurrency <= 0 {
		errs = append(errs, fmt.Errorf("PULL_CONCURRENCY must be positive, got %d", cfg.PullConcurrency))
	}
	// Zero means "no deadline" for the initial pull; only negatives are invalid.
	if cfg.InitialPullDeadline < 0 {
		errs = append(errs, fmt.Errorf("INITIAL_PULL_DEADLINE must not be negative, got %s", cfg.InitialPullDeadline))
	}
	if cfg.InitialPullBackoffMin <= 0 || cfg.InitialPullBackoffMax < cfg.InitialPullBackoffMin {
		errs = append(errs, fmt.Errorf("INITIAL_PULL_BACKOFF_MIN must be positive and not exceed INITIAL_PULL_BACKOFF_MAX, got %s and %s",
			cfg.InitialPullBackoffMin, cfg.InitialPullBackoffMax))
	}
	if cfg.PeerPollInterval <= 0 {
		errs = append(errs, fmt.Errorf("PEER_POLL_INTERVAL must be positive, got %s", cfg.PeerPollInterval))
	}
	if cfg.MaxCacheAge < 0 {
		errs = append(errs, fmt.Errorf("MAX_CACHE_AGE must not be negative, got %s", cfg.MaxCacheAge))
	}

	switch cfg.LeaderElection {
	case electionLease:
		// The lease must outlive a missed renewal, otherwise a primary that
		// pauses briefly loses the lease to a peer on every hiccup.
		if cfg.LeaseRenewInterval <= 0 || cfg.LeaseDuration <= cfg.LeaseRenewInterval {
			errs = append(errs, fmt.Errorf("LEASE_DURATION must exceed LEASE_RENEW_INTERVAL, got %s and %s",
				cfg.LeaseDuration, cfg.LeaseRenewInterval))
		}
	case electionOff:
	default:
		errs = append(errs, fmt.Errorf("invalid LEADER_ELECTION: %q (valid: lease, off)", cfg.LeaderElection))
	}

	if !slices.Contains(validLogLevels, cfg.LogLevel) {
		errs = append(errs, fmt.Errorf("invalid LOG_LEVEL: %q (valid: debug, info, warn, error)", cfg.LogLevel))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

func (c Config) AppIDs() []string {
	ids := make([]string, 0, len(c.Apps))
	for _, a := range c.Apps {
		ids = append(ids, a.ID)
	}
	return ids
}

func envOr(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

func hostnameOr(def string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return def
}
