package main

import (
	"strings"
	"testing"
	"time"
)

// ctAllConfigEnvVars is every env var LoadConfig reads. Each test sets all of
// them explicitly ("" simulates unset) to shield tests from the host env.
var ctAllConfigEnvVars = []string{
	"TRANSLATION_SOURCE",
	"TRANSLATION_BASE_URL",
	"TRANSLATION_APPLICATION_ID",
	"TRANSLATION_API_KEY",
	"TRANSLATION_XLSX_URL",
	"LISTEN_ADDR",
	"SQLITE_PATH",
	"HTTP_TIMEOUT",
	"PULL_INTERVAL",
	"INITIAL_PULL_DEADLINE",
	"LOG_LEVEL",
}

// ctSetConfigEnv sets every config env var, using values from overrides and
// "" for anything not listed there.
func ctSetConfigEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	for _, k := range ctAllConfigEnvVars {
		t.Setenv(k, overrides[k])
	}
}

// ctRequiredEnv sets only the three required vars to placeholder values.
func ctRequiredEnv() map[string]string {
	return map[string]string{
		"TRANSLATION_BASE_URL":       "https://translate.example.com",
		"TRANSLATION_APPLICATION_ID": "app-id",
		"TRANSLATION_API_KEY":        "api-key",
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	ctSetConfigEnv(t, ctRequiredEnv())

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil", err)
	}

	if got, want := cfg.TranslationBaseURL, "https://translate.example.com"; got != want {
		t.Errorf("TranslationBaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.TranslationApplicationID, "app-id"; got != want {
		t.Errorf("TranslationApplicationID = %q, want %q", got, want)
	}
	if got, want := cfg.TranslationAPIKey, "api-key"; got != want {
		t.Errorf("TranslationAPIKey = %q, want %q", got, want)
	}
	if got, want := cfg.ListenAddr, ":8080"; got != want {
		t.Errorf("ListenAddr = %q, want %q", got, want)
	}
	if got, want := cfg.SQLitePath, "/data/cacheppuccino.db"; got != want {
		t.Errorf("SQLitePath = %q, want %q", got, want)
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
	if got, want := cfg.TranslationSource, "api"; got != want {
		t.Errorf("TranslationSource = %q, want %q", got, want)
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
			ctSetConfigEnv(t, tt.env)

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
			if cfg.TranslationSource != tt.env["TRANSLATION_SOURCE"] {
				t.Errorf("TranslationSource = %q, want %q", cfg.TranslationSource, tt.env["TRANSLATION_SOURCE"])
			}
			if cfg.TranslationXLSXURL != tt.env["TRANSLATION_XLSX_URL"] {
				t.Errorf("TranslationXLSXURL = %q, want %q", cfg.TranslationXLSXURL, tt.env["TRANSLATION_XLSX_URL"])
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
			env := ctRequiredEnv()
			env[missing] = ""
			ctSetConfigEnv(t, env)

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
		ctSetConfigEnv(t, nil)

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
			env := ctRequiredEnv()
			for k, v := range tt.overrides {
				env[k] = v
			}
			ctSetConfigEnv(t, env)

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
	env := ctRequiredEnv()
	env["TRANSLATION_API_KEY"] = ""
	env["PULL_INTERVAL"] = "bogus"
	env["LOG_LEVEL"] = "loud"
	ctSetConfigEnv(t, env)

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
	env := ctRequiredEnv()
	env["INITIAL_PULL_DEADLINE"] = "0s"
	ctSetConfigEnv(t, env)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil", err)
	}
	if cfg.InitialPullDeadline != 0 {
		t.Errorf("InitialPullDeadline = %v, want 0", cfg.InitialPullDeadline)
	}
}

func TestLoadConfigValidOverrides(t *testing.T) {
	env := ctRequiredEnv()
	env["LISTEN_ADDR"] = "127.0.0.1:9999"
	env["SQLITE_PATH"] = "/tmp/other.db"
	env["HTTP_TIMEOUT"] = "5s"
	env["PULL_INTERVAL"] = "1h30m"
	env["INITIAL_PULL_DEADLINE"] = "250ms"
	env["LOG_LEVEL"] = "debug"
	ctSetConfigEnv(t, env)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil", err)
	}

	if got, want := cfg.ListenAddr, "127.0.0.1:9999"; got != want {
		t.Errorf("ListenAddr = %q, want %q", got, want)
	}
	if got, want := cfg.SQLitePath, "/tmp/other.db"; got != want {
		t.Errorf("SQLitePath = %q, want %q", got, want)
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
			env := ctRequiredEnv()
			env["LOG_LEVEL"] = level
			ctSetConfigEnv(t, env)

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
