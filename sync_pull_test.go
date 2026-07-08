package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

const stSheetName = "Translations"

func stLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func stOpenDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func stClient(baseURL, apiKey string) *TranslationClient {
	return NewTranslationClient(Config{
		TranslationBaseURL:       baseURL,
		TranslationApplicationID: "app-1",
		TranslationAPIKey:        apiKey,
		HTTPTimeout:              5 * time.Second,
	})
}

// stXLSX builds an in-memory workbook whose "Translations" sheet holds the
// given rows; rows[0] is the header.
func stXLSX(t *testing.T, rows [][]string) []byte {
	t.Helper()

	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	if err := f.SetSheetName("Sheet1", stSheetName); err != nil {
		t.Fatalf("SetSheetName: %v", err)
	}
	for i := range rows {
		cell, err := excelize.CoordinatesToCellName(1, i+1)
		if err != nil {
			t.Fatalf("CoordinatesToCellName: %v", err)
		}
		if err := f.SetSheetRow(stSheetName, cell, &rows[i]); err != nil {
			t.Fatalf("SetSheetRow: %v", err)
		}
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatalf("WriteToBuffer: %v", err)
	}
	return buf.Bytes()
}

// stSeedPayload has 3 data rows x 2 languages = 6 imported StringRows.
func stSeedPayload(t *testing.T) []byte {
	t.Helper()
	return stXLSX(t, [][]string{
		{"page", "key", "en", "fr"},
		{"home", "greeting", "Hello", "Bonjour"},
		{"home", "farewell", "Goodbye", "Au revoir"},
		{"about", "title", "About us", "A propos"},
	})
}

func stSeedWantEN() map[string]map[string]string {
	return map[string]map[string]string{
		"home":  {"greeting": "Hello", "farewell": "Goodbye"},
		"about": {"title": "About us"},
	}
}

func TestTranslationClientDownloadXLSXRequestShape(t *testing.T) {
	cases := []struct {
		name       string
		apiKey     string
		wantHeader bool
	}{
		{name: "api key set", apiKey: "secret", wantHeader: true},
		{name: "api key empty", apiKey: "", wantHeader: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu         sync.Mutex
				gotMethod  string
				gotPath    string
				gotKey     string
				headerSeen bool
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotKey = r.Header.Get("X-API-KEY")
				_, headerSeen = r.Header[http.CanonicalHeaderKey("X-API-KEY")]
				mu.Unlock()
				_, _ = w.Write([]byte("body-bytes"))
			}))
			defer srv.Close()

			client := stClient(srv.URL, tc.apiKey)
			body, err := client.Fetch(context.Background(), stLogger())
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if string(body) != "body-bytes" {
				t.Errorf("body = %q, want %q", body, "body-bytes")
			}

			mu.Lock()
			defer mu.Unlock()
			if gotMethod != http.MethodGet {
				t.Errorf("method = %q, want GET", gotMethod)
			}
			if want := "/api/Application/app-1/Translation/export"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
			if tc.wantHeader {
				if !headerSeen || gotKey != tc.apiKey {
					t.Errorf("X-API-KEY = %q (present=%v), want %q", gotKey, headerSeen, tc.apiKey)
				}
			} else if headerSeen {
				t.Errorf("X-API-KEY header present (%q), want absent", gotKey)
			}
		})
	}
}

func TestPullOnceFirstThenSkipThenReplace(t *testing.T) {
	ctx := context.Background()
	db := stOpenDB(t)
	logger := stLogger()

	payload1 := stSeedPayload(t)
	// v2: "home"/"farewell" removed, "contact"/"email" added.
	payload2 := stXLSX(t, [][]string{
		{"page", "key", "en", "fr"},
		{"home", "greeting", "Hello", "Bonjour"},
		{"about", "title", "About us", "A propos"},
		{"contact", "email", "Email", "Courriel"},
	})

	var mu sync.Mutex
	payload := payload1
	setPayload := func(p []byte) {
		mu.Lock()
		payload = p
		mu.Unlock()
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		p := payload
		mu.Unlock()
		_, _ = w.Write(p)
	}))
	defer srv.Close()

	client := stClient(srv.URL, "secret")

	// First pull: full import.
	res1, err := PullOnce(ctx, db, client, logger)
	if err != nil {
		t.Fatalf("first PullOnce: %v", err)
	}
	if res1.Skipped {
		t.Error("first pull: Skipped = true, want false")
	}
	if res1.Rows != 6 {
		t.Errorf("first pull: Rows = %d, want 6", res1.Rows)
	}
	if want := HashBytes(payload1); res1.Hash != want {
		t.Errorf("first pull: Hash = %q, want %q", res1.Hash, want)
	}

	gotEN, _, err := db.GetStringsByPagesLang(ctx, []string{"home", "about"}, "en")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang(en): %v", err)
	}
	if want := stSeedWantEN(); !reflect.DeepEqual(gotEN, want) {
		t.Errorf("en strings after first pull = %v, want %v", gotEN, want)
	}
	gotFR, _, err := db.GetStringsByPagesLang(ctx, []string{"home"}, "fr")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang(fr): %v", err)
	}
	wantFR := map[string]map[string]string{
		"home": {"greeting": "Bonjour", "farewell": "Au revoir"},
	}
	if !reflect.DeepEqual(gotFR, wantFR) {
		t.Errorf("fr strings after first pull = %v, want %v", gotFR, wantFR)
	}

	hashMeta, ok, err := db.GetMeta(ctx, metaKeyLastHash)
	if err != nil {
		t.Fatalf("GetMeta(%s): %v", metaKeyLastHash, err)
	}
	if !ok || hashMeta != res1.Hash {
		t.Errorf("meta %s = %q (ok=%v), want %q", metaKeyLastHash, hashMeta, ok, res1.Hash)
	}
	pullMeta, ok, err := db.GetMeta(ctx, metaKeyLastPull)
	if err != nil {
		t.Fatalf("GetMeta(%s): %v", metaKeyLastPull, err)
	}
	if !ok {
		t.Fatalf("meta %s not set after first pull", metaKeyLastPull)
	}
	if _, err := time.Parse(time.RFC3339, pullMeta); err != nil {
		t.Errorf("meta %s = %q is not RFC3339: %v", metaKeyLastPull, pullMeta, err)
	}

	// Second pull with identical bytes: skipped, but last pull refreshed.
	const stale = "2000-01-01T00:00:00Z"
	if err := db.SetMeta(ctx, metaKeyLastPull, stale); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	res2, err := PullOnce(ctx, db, client, logger)
	if err != nil {
		t.Fatalf("second PullOnce: %v", err)
	}
	if !res2.Skipped {
		t.Error("second pull: Skipped = false, want true")
	}
	if res2.Rows != 0 {
		t.Errorf("second pull: Rows = %d, want 0", res2.Rows)
	}
	if res2.Hash != res1.Hash {
		t.Errorf("second pull: Hash = %q, want %q", res2.Hash, res1.Hash)
	}

	pullMeta2, ok, err := db.GetMeta(ctx, metaKeyLastPull)
	if err != nil {
		t.Fatalf("GetMeta(%s): %v", metaKeyLastPull, err)
	}
	if !ok {
		t.Fatalf("meta %s missing after skipped pull", metaKeyLastPull)
	}
	if pullMeta2 == stale {
		t.Errorf("meta %s = %q, want refreshed even on skipped pull", metaKeyLastPull, pullMeta2)
	}
	ts, err := time.Parse(time.RFC3339, pullMeta2)
	if err != nil {
		t.Fatalf("meta %s = %q is not RFC3339: %v", metaKeyLastPull, pullMeta2, err)
	}
	if !ts.After(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("meta %s = %q, want a recent timestamp", metaKeyLastPull, pullMeta2)
	}

	// Third pull with changed payload: full replacement, removed row gone.
	setPayload(payload2)

	res3, err := PullOnce(ctx, db, client, logger)
	if err != nil {
		t.Fatalf("third PullOnce: %v", err)
	}
	if res3.Skipped {
		t.Error("third pull: Skipped = true, want false")
	}
	if res3.Rows != 6 {
		t.Errorf("third pull: Rows = %d, want 6", res3.Rows)
	}
	if want := HashBytes(payload2); res3.Hash != want {
		t.Errorf("third pull: Hash = %q, want %q", res3.Hash, want)
	}
	if res3.Hash == res1.Hash {
		t.Error("third pull: hash unchanged despite different payload")
	}

	gotEN3, _, err := db.GetStringsByPagesLang(ctx, []string{"home", "about", "contact"}, "en")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang(en): %v", err)
	}
	wantEN3 := map[string]map[string]string{
		"home":    {"greeting": "Hello"},
		"about":   {"title": "About us"},
		"contact": {"email": "Email"},
	}
	if !reflect.DeepEqual(gotEN3, wantEN3) {
		t.Errorf("en strings after third pull = %v, want %v (removed row must be gone)", gotEN3, wantEN3)
	}

	hashMeta3, ok, err := db.GetMeta(ctx, metaKeyLastHash)
	if err != nil {
		t.Fatalf("GetMeta(%s): %v", metaKeyLastHash, err)
	}
	if !ok || hashMeta3 != res3.Hash {
		t.Errorf("meta %s = %q (ok=%v), want %q", metaKeyLastHash, hashMeta3, ok, res3.Hash)
	}
}

func TestPullOnceUpstreamHTTPError(t *testing.T) {
	ctx := context.Background()
	db := stOpenDB(t)
	logger := stLogger()
	payload := stSeedPayload(t)

	var mu sync.Mutex
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		f := fail
		mu.Unlock()
		if f {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("upstream exploded"))
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	client := stClient(srv.URL, "secret")

	res, err := PullOnce(ctx, db, client, logger)
	if err != nil {
		t.Fatalf("seed PullOnce: %v", err)
	}

	mu.Lock()
	fail = true
	mu.Unlock()

	_, err = PullOnce(ctx, db, client, logger)
	if err == nil {
		t.Fatal("PullOnce on upstream 500: want error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to mention status 500", err)
	}

	hashMeta, ok, err := db.GetMeta(ctx, metaKeyLastHash)
	if err != nil {
		t.Fatalf("GetMeta(%s): %v", metaKeyLastHash, err)
	}
	if !ok || hashMeta != res.Hash {
		t.Errorf("meta %s = %q (ok=%v), want unchanged %q", metaKeyLastHash, hashMeta, ok, res.Hash)
	}
	gotEN, _, err := db.GetStringsByPagesLang(ctx, []string{"home", "about"}, "en")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang: %v", err)
	}
	if want := stSeedWantEN(); !reflect.DeepEqual(gotEN, want) {
		t.Errorf("en strings after failed pull = %v, want unchanged %v", gotEN, want)
	}
}

func TestPullOnceInvalidXLSXBody(t *testing.T) {
	ctx := context.Background()
	db := stOpenDB(t)
	logger := stLogger()
	payload := stSeedPayload(t)

	var mu sync.Mutex
	junk := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		j := junk
		mu.Unlock()
		if j {
			_, _ = w.Write([]byte("definitely not a zip archive"))
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	client := stClient(srv.URL, "secret")

	res, err := PullOnce(ctx, db, client, logger)
	if err != nil {
		t.Fatalf("seed PullOnce: %v", err)
	}

	mu.Lock()
	junk = true
	mu.Unlock()

	_, err = PullOnce(ctx, db, client, logger)
	if err == nil {
		t.Fatal("PullOnce on junk body: want parse error, got nil")
	}
	if strings.Contains(err.Error(), "download failed") {
		t.Errorf("error = %q, want a parse error, not a download error", err)
	}

	hashMeta, ok, err := db.GetMeta(ctx, metaKeyLastHash)
	if err != nil {
		t.Fatalf("GetMeta(%s): %v", metaKeyLastHash, err)
	}
	if !ok || hashMeta != res.Hash {
		t.Errorf("meta %s = %q (ok=%v), want unchanged %q", metaKeyLastHash, hashMeta, ok, res.Hash)
	}
	gotEN, _, err := db.GetStringsByPagesLang(ctx, []string{"home", "about"}, "en")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang: %v", err)
	}
	if want := stSeedWantEN(); !reflect.DeepEqual(gotEN, want) {
		t.Errorf("en strings after failed pull = %v, want unchanged %v", gotEN, want)
	}
}

func TestPullOnceContextCancelled(t *testing.T) {
	db := stOpenDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("never used"))
	}))
	defer srv.Close()

	client := stClient(srv.URL, "secret")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := PullOnce(ctx, db, client, stLogger())
	if err == nil {
		t.Fatal("PullOnce with cancelled context: want error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want errors.Is(err, context.Canceled)", err)
	}
}
