package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// allConfigEnvVars is every env var LoadConfig reads. Each test takes control
// of all of them so the host environment cannot change a result.
var allConfigEnvVars = []string{
	"TRANSLATION_SOURCE",
	"TRANSLATION_BASE_URL",
	"TRANSLATION_APPLICATION_ID",
	"TRANSLATION_API_KEY",
	"TRANSLATION_XLSX_URL",
	"LISTEN_ADDR",
	"INTERNAL_LISTEN_ADDR",
	"LOG_LEVEL",
	"CACHE_DIR",
	"MAX_CACHE_AGE",
	"PEER_SERVICE",
	"PEER_POLL_INTERVAL",
	"PEER_TIMEOUT",
	"LEADER_ELECTION",
	"LEASE_NAME",
	"LEASE_DURATION",
	"LEASE_RENEW_INTERVAL",
	"PULL_INTERVAL",
	"PULL_CONCURRENCY",
	"HTTP_TIMEOUT",
	"INITIAL_PULL_DEADLINE",
	"INITIAL_PULL_BACKOFF_MIN",
	"INITIAL_PULL_BACKOFF_MAX",
	"ALARM_NO_PRIMARY",
	"ALARM_SNAPSHOT_AGE",
	"ALARM_DIVERGENCE",
	"ALARM_PEER_UNREADY",
	"ALARM_PULL_FAILURES",
	"CLUSTER_CACHE_TTL",
	"POD_NAME",
	"POD_NAMESPACE",
	"POD_IP",
}

// setConfigEnv applies overrides and unsets every other config var. Unset is
// not the same as empty -- CACHE_DIR reads an explicit "" as "no cache" --
// so anything absent from overrides is removed rather than blanked.
func setConfigEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	for _, k := range allConfigEnvVars {
		if v, ok := overrides[k]; ok {
			t.Setenv(k, v)
			continue
		}
		// Setenv registers the cleanup that restores the original value;
		// Unsetenv then gives this test a genuinely absent variable.
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("Unsetenv(%s): %v", k, err)
		}
	}
}

// requiredAPIEnv sets only the three required vars to placeholder values.
func requiredAPIEnv() map[string]string {
	return map[string]string{
		"TRANSLATION_BASE_URL":       "https://translate.example.com",
		"TRANSLATION_APPLICATION_ID": "app-id",
		"TRANSLATION_API_KEY":        "api-key",
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	setConfigEnv(t, requiredAPIEnv())

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil", err)
	}

	app := onlyApp(t, cfg)
	if got, want := app.BaseURL, "https://translate.example.com"; got != want {
		t.Errorf("app.BaseURL = %q, want %q", got, want)
	}
	if got, want := app.ApplicationID, "app-id"; got != want {
		t.Errorf("app.ApplicationID = %q, want %q", got, want)
	}
	if got, want := app.APIKey, "api-key"; got != want {
		t.Errorf("app.APIKey = %q, want %q", got, want)
	}
	if got, want := cfg.ListenAddr, ":8080"; got != want {
		t.Errorf("ListenAddr = %q, want %q", got, want)
	}
	if got, want := cfg.CacheDir, "/cache"; got != want {
		t.Errorf("CacheDir = %q, want %q", got, want)
	}
	if got, want := cfg.InternalListenAddr, ":8081"; got != want {
		t.Errorf("InternalListenAddr = %q, want %q", got, want)
	}
	if got, want := cfg.LeaderElection, electionOff; got != want {
		t.Errorf("LeaderElection = %q, want %q", got, want)
	}
	// Zero ALARM_SNAPSHOT_AGE derives from the pull interval rather than
	// silently staying at a threshold that ignores it.
	if got, want := cfg.AlarmSnapshotAge, 40*time.Minute; got != want {
		t.Errorf("AlarmSnapshotAge = %v, want %v", got, want)
	}
	if got, want := cfg.HTTPTimeout, 30*time.Second; got != want {
		t.Errorf("HTTPTimeout = %v, want %v", got, want)
	}
	if got, want := cfg.PullInterval, 10*time.Minute; got != want {
		t.Errorf("PullInterval = %v, want %v", got, want)
	}
	if got, want := cfg.InitialPullDeadline, 45*time.Second; got != want {
		t.Errorf("InitialPullDeadline = %v, want %v", got, want)
	}
	if got, want := cfg.LogLevel, "info"; got != want {
		t.Errorf("LogLevel = %q, want %q", got, want)
	}
	if got, want := app.Source, "api"; got != want {
		t.Errorf("app.Source = %q, want %q", got, want)
	}
}

func TestLoadConfigSourceMatrix(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantErr     bool
		wantInError []string
	}{
		{
			name: "url mode requires only the xlsx url",
			env: map[string]string{
				"TRANSLATION_SOURCE":   "url",
				"TRANSLATION_XLSX_URL": "https://files.example.com/translations.xlsx",
			},
		},
		{
			name:        "url mode without xlsx url fails",
			env:         map[string]string{"TRANSLATION_SOURCE": "url"},
			wantErr:     true,
			wantInError: []string{"TRANSLATION_XLSX_URL"},
		},
		{
			name: "url mode rejects non-http url",
			env: map[string]string{
				"TRANSLATION_SOURCE":   "url",
				"TRANSLATION_XLSX_URL": "ftp://files.example.com/translations.xlsx",
			},
			wantErr:     true,
			wantInError: []string{"TRANSLATION_XLSX_URL", "http(s)"},
		},
		{
			name:        "invalid source value fails",
			env:         map[string]string{"TRANSLATION_SOURCE": "s3"},
			wantErr:     true,
			wantInError: []string{"TRANSLATION_SOURCE", `"s3"`},
		},
		{
			name: "api mode still requires the api trio",
			env: map[string]string{
				"TRANSLATION_SOURCE":   "api",
				"TRANSLATION_XLSX_URL": "https://files.example.com/translations.xlsx",
			},
			wantErr: true,
			wantInError: []string{
				"TRANSLATION_BASE_URL",
				"TRANSLATION_APPLICATION_ID",
				"TRANSLATION_API_KEY",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setConfigEnv(t, tt.env)

			cfg, err := LoadConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatal("LoadConfig() error = nil, want error")
				}
				for _, want := range tt.wantInError {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig() error = %v, want nil", err)
			}
			app := onlyApp(t, cfg)
			if app.Source != tt.env["TRANSLATION_SOURCE"] {
				t.Errorf("app.Source = %q, want %q", app.Source, tt.env["TRANSLATION_SOURCE"])
			}
			if app.XLSXURL != tt.env["TRANSLATION_XLSX_URL"] {
				t.Errorf("app.XLSXURL = %q, want %q", app.XLSXURL, tt.env["TRANSLATION_XLSX_URL"])
			}
		})
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	required := []string{
		"TRANSLATION_BASE_URL",
		"TRANSLATION_APPLICATION_ID",
		"TRANSLATION_API_KEY",
	}

	for _, missing := range required {
		t.Run(missing, func(t *testing.T) {
			env := requiredAPIEnv()
			env[missing] = ""
			setConfigEnv(t, env)

			_, err := LoadConfig()
			if err == nil {
				t.Fatalf("LoadConfig() error = nil, want error mentioning %s", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error %q does not mention %s", err.Error(), missing)
			}
		})
	}

	t.Run("all missing", func(t *testing.T) {
		setConfigEnv(t, nil)

		_, err := LoadConfig()
		if err == nil {
			t.Fatal("LoadConfig() error = nil, want error mentioning all required vars")
		}
		for _, k := range required {
			if !strings.Contains(err.Error(), k) {
				t.Errorf("error %q does not mention %s", err.Error(), k)
			}
		}
	})
}

func TestLoadConfigInvalidValues(t *testing.T) {
	tests := []struct {
		name        string
		overrides   map[string]string
		wantInError []string
	}{
		{
			name:        "invalid PULL_INTERVAL",
			overrides:   map[string]string{"PULL_INTERVAL": "abc"},
			wantInError: []string{"PULL_INTERVAL", `"abc"`},
		},
		{
			name:        "invalid HTTP_TIMEOUT",
			overrides:   map[string]string{"HTTP_TIMEOUT": "thirty"},
			wantInError: []string{"HTTP_TIMEOUT", `"thirty"`},
		},
		{
			name:        "invalid INITIAL_PULL_DEADLINE",
			overrides:   map[string]string{"INITIAL_PULL_DEADLINE": "45"},
			wantInError: []string{"INITIAL_PULL_DEADLINE", `"45"`},
		},
		{
			name:        "invalid LOG_LEVEL",
			overrides:   map[string]string{"LOG_LEVEL": "verbose"},
			wantInError: []string{"LOG_LEVEL", `"verbose"`},
		},
		{
			name:        "zero PULL_INTERVAL",
			overrides:   map[string]string{"PULL_INTERVAL": "0s"},
			wantInError: []string{"PULL_INTERVAL", "positive"},
		},
		{
			name:        "negative PULL_INTERVAL",
			overrides:   map[string]string{"PULL_INTERVAL": "-10m"},
			wantInError: []string{"PULL_INTERVAL", "positive"},
		},
		{
			name:        "zero HTTP_TIMEOUT",
			overrides:   map[string]string{"HTTP_TIMEOUT": "0"},
			wantInError: []string{"HTTP_TIMEOUT", "positive"},
		},
		{
			name:        "negative INITIAL_PULL_DEADLINE",
			overrides:   map[string]string{"INITIAL_PULL_DEADLINE": "-1s"},
			wantInError: []string{"INITIAL_PULL_DEADLINE", "negative"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := requiredAPIEnv()
			for k, v := range tt.overrides {
				env[k] = v
			}
			setConfigEnv(t, env)

			_, err := LoadConfig()
			if err == nil {
				t.Fatalf("LoadConfig() error = nil, want error mentioning %v", tt.wantInError)
			}
			for _, want := range tt.wantInError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestLoadConfigMultipleProblems(t *testing.T) {
	// Missing one required var plus a bad duration plus a bad log level:
	// all must be reported in the single joined error.
	env := requiredAPIEnv()
	env["TRANSLATION_API_KEY"] = ""
	env["PULL_INTERVAL"] = "bogus"
	env["LOG_LEVEL"] = "loud"
	setConfigEnv(t, env)

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want joined error with all problems")
	}
	for _, want := range []string{
		"TRANSLATION_API_KEY",
		"PULL_INTERVAL", `"bogus"`,
		"LOG_LEVEL", `"loud"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

func TestLoadConfigZeroInitialPullDeadline(t *testing.T) {
	// Zero means "no deadline" and must be accepted.
	env := requiredAPIEnv()
	env["INITIAL_PULL_DEADLINE"] = "0s"
	setConfigEnv(t, env)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil", err)
	}
	if cfg.InitialPullDeadline != 0 {
		t.Errorf("InitialPullDeadline = %v, want 0", cfg.InitialPullDeadline)
	}
}

func TestLoadConfigValidOverrides(t *testing.T) {
	env := requiredAPIEnv()
	env["LISTEN_ADDR"] = "127.0.0.1:9999"
	env["CACHE_DIR"] = "/tmp/other-cache"
	env["HTTP_TIMEOUT"] = "5s"
	env["PULL_INTERVAL"] = "1h30m"
	env["INITIAL_PULL_DEADLINE"] = "250ms"
	env["LOG_LEVEL"] = "debug"
	setConfigEnv(t, env)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil", err)
	}

	if got, want := cfg.ListenAddr, "127.0.0.1:9999"; got != want {
		t.Errorf("ListenAddr = %q, want %q", got, want)
	}
	if got, want := cfg.CacheDir, "/tmp/other-cache"; got != want {
		t.Errorf("CacheDir = %q, want %q", got, want)
	}
	if got, want := cfg.HTTPTimeout, 5*time.Second; got != want {
		t.Errorf("HTTPTimeout = %v, want %v", got, want)
	}
	if got, want := cfg.PullInterval, 90*time.Minute; got != want {
		t.Errorf("PullInterval = %v, want %v", got, want)
	}
	if got, want := cfg.InitialPullDeadline, 250*time.Millisecond; got != want {
		t.Errorf("InitialPullDeadline = %v, want %v", got, want)
	}
	if got, want := cfg.LogLevel, "debug"; got != want {
		t.Errorf("LogLevel = %q, want %q", got, want)
	}
}

func TestLoadConfigValidLogLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			env := requiredAPIEnv()
			env["LOG_LEVEL"] = level
			setConfigEnv(t, env)

			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig() error = %v, want nil", err)
			}
			if cfg.LogLevel != level {
				t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, level)
			}
		})
	}
}

// onlyApp returns the single configured application, failing if the config
// somehow carries a different number.
func onlyApp(t *testing.T, cfg Config) AppConfig {
	t.Helper()

	if len(cfg.Apps) != 1 {
		t.Fatalf("len(cfg.Apps) = %d, want 1", len(cfg.Apps))
	}
	return cfg.Apps[0]
}
