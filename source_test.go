package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testURLSource(rawURL string) *URLSource {
	return NewURLSource(Config{
		TranslationXLSXURL: rawURL,
		HTTPTimeout:        5 * time.Second,
	})
}

func TestURLSourceFetch(t *testing.T) {
	t.Run("plain GET, no auth header", func(t *testing.T) {
		var gotPath, gotKey string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotKey = r.Header.Get("X-API-KEY")
			_, _ = w.Write([]byte("xlsx-bytes"))
		}))
		defer srv.Close()

		src := testURLSource(srv.URL + "/files/translations.xlsx")
		body, err := src.Fetch(context.Background(), discardLogger())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if string(body) != "xlsx-bytes" {
			t.Errorf("body = %q, want %q", body, "xlsx-bytes")
		}
		if gotPath != "/files/translations.xlsx" {
			t.Errorf("path = %q, want /files/translations.xlsx", gotPath)
		}
		if gotKey != "" {
			t.Errorf("X-API-KEY sent (%q), want none", gotKey)
		}
		if src.Name() != "url" {
			t.Errorf("Name() = %q, want url", src.Name())
		}
	})

	t.Run("non-2xx fails with status and body excerpt", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no such blob", http.StatusNotFound)
		}))
		defer srv.Close()

		_, err := testURLSource(srv.URL).Fetch(context.Background(), discardLogger())
		if err == nil {
			t.Fatal("Fetch on 404: want error, got nil")
		}
		if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "no such blob") {
			t.Errorf("error %q should mention status and body", err.Error())
		}
	})

	t.Run("transport error redacts query credentials", func(t *testing.T) {
		// Errors end up on the public /status endpoint; a SAS/presigned
		// token in the query must never appear there.
		src := testURLSource("http://127.0.0.1:1/translations.xlsx?sv=2022-11-02&sig=SUPERSECRETSAS")
		_, err := src.Fetch(context.Background(), discardLogger())
		if err == nil {
			t.Fatal("Fetch on unreachable host: want error, got nil")
		}
		if strings.Contains(err.Error(), "SUPERSECRETSAS") {
			t.Errorf("error %q leaks the query credential", err.Error())
		}
		if !strings.Contains(err.Error(), "/translations.xlsx") {
			t.Errorf("error %q should keep host/path for diagnosis", err.Error())
		}
	})

	t.Run("error body echoing query credentials is masked", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "signature mismatch for sig=SUPERSECRETSAS", http.StatusForbidden)
		}))
		defer srv.Close()

		src := testURLSource(srv.URL + "/translations.xlsx?sig=SUPERSECRETSAS")
		_, err := src.Fetch(context.Background(), discardLogger())
		if err == nil {
			t.Fatal("Fetch on 403: want error, got nil")
		}
		if strings.Contains(err.Error(), "SUPERSECRETSAS") {
			t.Errorf("error %q leaks the credential echoed by the body", err.Error())
		}
		if !strings.Contains(err.Error(), "403") {
			t.Errorf("error %q should keep the status", err.Error())
		}
	})
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://blob.example.com/c/translations.xlsx?sv=1&sig=SECRET", "https://blob.example.com/c/translations.xlsx"},
		{"https://files.example.com/translations.xlsx", "https://files.example.com/translations.xlsx"},
		{"https://user:pass@files.example.com/t.xlsx#frag", "https://files.example.com/t.xlsx"},
	}
	for _, tt := range tests {
		if got := redactURL(tt.in); got != tt.want {
			t.Errorf("redactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsHTTPSuccess(t *testing.T) {
	tests := []struct {
		statusCode int
		want       bool
	}{
		{199, false},
		{200, true},
		{204, true},
		{299, true},
		{300, false},
		{304, false},
		{404, false},
		{500, false},
	}
	for _, tt := range tests {
		if got := isHTTPSuccess(tt.statusCode); got != tt.want {
			t.Errorf("isHTTPSuccess(%d) = %v, want %v", tt.statusCode, got, tt.want)
		}
	}
}

func TestReadAllLimited(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		max     int64
		wantErr bool
	}{
		{name: "under limit", input: "12345", max: 10},
		{name: "exactly at limit", input: "1234567890", max: 10},
		{name: "over limit", input: "12345678901", max: 10, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readAllLimited(strings.NewReader(tt.input), tt.max)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("readAllLimited = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("readAllLimited: %v", err)
			}
			if string(got) != tt.input {
				t.Errorf("readAllLimited = %q, want %q", got, tt.input)
			}
		})
	}
}

// TestStatusReportsSourceAndPullOutcome covers the /status additions for QA
// self-diagnosis: source kind, last pull error, and last import row count.
func TestStatusReportsSourceAndPullOutcome(t *testing.T) {
	ctx := context.Background()

	db := openTestDB(t)
	ready := &ReadyState{}
	ready.SetReady(true)
	srv := &Server{db: db, ready: ready, logger: discardLogger(), sourceName: "url"}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	getStatus := func() StatusResponse {
		t.Helper()
		resp, err := http.Get(ts.URL + "/status")
		if err != nil {
			t.Fatalf("GET /status: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /status = %d, want 200", resp.StatusCode)
		}
		var envelope struct {
			Ok   bool           `json:"ok"`
			Data StatusResponse `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode /status: %v", err)
		}
		return envelope.Data
	}

	// Fresh DB: source is reported, counters empty.
	st := getStatus()
	if st.Source != "url" {
		t.Errorf("source = %q, want url", st.Source)
	}
	if st.LastPullError != "" || st.LastImportRows != 0 {
		t.Errorf("fresh status = %+v, want empty error and 0 rows", st)
	}

	// After an import: row count present.
	rows := []StringRow{
		{Page: "home", Key: "a", Lang: "en", Value: "A", UpdatedAt: time.Now()},
		{Page: "home", Key: "b", Lang: "en", Value: "B", UpdatedAt: time.Now()},
	}
	if err := db.ReplaceImport(ctx, rows, "hash-1", time.Now()); err != nil {
		t.Fatalf("ReplaceImport: %v", err)
	}
	st = getStatus()
	if st.LastImportRows != 2 {
		t.Errorf("last_import_rows = %d, want 2", st.LastImportRows)
	}
	if st.LastHash != "hash-1" {
		t.Errorf("last_hash = %q, want hash-1", st.LastHash)
	}

	// A recorded pull failure surfaces, then clears.
	if err := db.SetMeta(ctx, metaKeyLastPullError, "download failed: 404"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if st = getStatus(); st.LastPullError != "download failed: 404" {
		t.Errorf("last_pull_error = %q, want the recorded error", st.LastPullError)
	}
	if err := db.SetMeta(ctx, metaKeyLastPullError, ""); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if st = getStatus(); st.LastPullError != "" {
		t.Errorf("last_pull_error = %q, want cleared", st.LastPullError)
	}
}
