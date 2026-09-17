package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

func testAPISource(baseURL, apiKey string) *APISource {
	return NewAPISource(AppConfig{
		BaseURL:       baseURL,
		ApplicationID: "app-1",
		APIKey:        apiKey,
	}, 5*time.Second)
}

// seedXLSX has 3 data rows x 2 languages = 6 imported StringRows.
func seedXLSX(t *testing.T) []byte {
	t.Helper()
	return translationsXLSX(t, [][]string{
		{"page", "key", "en", "fr"},
		{"home", "greeting", "Hello", "Bonjour"},
		{"home", "farewell", "Goodbye", "Au revoir"},
		{"about", "title", "About us", "A propos"},
	})
}

func seedWantEN() map[string]map[string]string {
	return map[string]map[string]string{
		"home":  {"greeting": "Hello", "farewell": "Goodbye"},
		"about": {"title": "About us"},
	}
}

func TestAPISourceDownloadXLSXRequestShape(t *testing.T) {
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

			client := testAPISource(srv.URL, tc.apiKey)
			body, err := client.Fetch(context.Background(), discardLogger())
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

func TestPullAppFirstThenSkipThenReplace(t *testing.T) {
	ctx := context.Background()
	src := &stubSource{body: seedXLSX(t)}
	syncer, registry := newTestSyncer(t, src)
	holder, _ := registry.Holder(defaultAppID)

	syncer.pullApp(ctx, defaultAppID, "test")

	first := holder.Load()
	if first == nil {
		t.Fatalf("no snapshot after first pull")
	}
	if first.RowCount != 6 {
		t.Errorf("rows = %d, want 6", first.RowCount)
	}
	if got, _ := first.Get([]string{"home", "about"}, "en"); !reflect.DeepEqual(toPlain(got), seedWantEN()) {
		t.Errorf("en strings = %v, want %v", toPlain(got), seedWantEN())
	}

	// Identical bytes must not rebuild the snapshot: the pointer staying put
	// is what lets clients keep their cached ETag.
	syncer.pullApp(ctx, defaultAppID, "test")
	if holder.Load() != first {
		t.Errorf("unchanged upstream replaced the snapshot")
	}

	// A removed row must disappear: upstream is the whole truth, not a delta.
	src.body = translationsXLSX(t, [][]string{
		{"page", "key", "en", "fr"},
		{"home", "greeting", "Hi", "Salut"},
	})
	syncer.pullApp(ctx, defaultAppID, "test")

	second := holder.Load()
	if second == first {
		t.Fatalf("changed upstream did not replace the snapshot")
	}
	if second.RowCount != 2 {
		t.Errorf("rows = %d, want 2", second.RowCount)
	}
	got, _ := second.Get([]string{"home", "about"}, "en")
	if got["home"]["greeting"] != "Hi" {
		t.Errorf("home.greeting = %q, want %q", got["home"]["greeting"], "Hi")
	}
	if _, present := got["home"]["farewell"]; present {
		t.Errorf("home.farewell survived an import that dropped it")
	}
	if len(got["about"]) != 0 {
		t.Errorf("about = %v, want empty after the page was dropped", got["about"])
	}
}

func TestPullAppFailuresKeepServingPreviousData(t *testing.T) {
	ctx := context.Background()

	// Parses cleanly and yields no rows: the errEmptyImport path, which is
	// not the same as an unreadable body. Built here rather than inside the
	// closure, which runs on a subtest's goroutine.
	headerOnly := translationsXLSX(t, [][]string{{"page", "key", "en"}})

	tests := []struct {
		name   string
		break_ func(*stubSource)
	}{
		{"upstream error", func(s *stubSource) { s.err = errors.New("502 bad gateway") }},
		{"unparseable body", func(s *stubSource) { s.body = []byte("not an xlsx") }},
		{"empty import", func(s *stubSource) { s.body = headerOnly }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := &stubSource{body: seedXLSX(t)}
			syncer, registry := newTestSyncer(t, src)
			holder, _ := registry.Holder(defaultAppID)

			syncer.pullApp(ctx, defaultAppID, "test")
			good := holder.Load()
			if good == nil {
				t.Fatalf("no snapshot after seed pull")
			}

			tc.break_(src)
			syncer.pullApp(ctx, defaultAppID, "test")

			if holder.Load() != good {
				t.Errorf("a failed pull replaced servable data")
			}
			_, lastErr, failures := syncer.state.App(defaultAppID).snapshot()
			if lastErr == "" {
				t.Errorf("last_pull_error is empty after a failed pull")
			}
			if failures != 1 {
				t.Errorf("consecutive_failures = %d, want 1", failures)
			}
		})
	}
}

func TestPullAppRecordsSuccessAfterFailure(t *testing.T) {
	ctx := context.Background()
	src := &stubSource{err: errors.New("down")}
	syncer, _ := newTestSyncer(t, src)

	syncer.pullApp(ctx, defaultAppID, "test")
	if _, lastErr, _ := syncer.state.App(defaultAppID).snapshot(); lastErr == "" {
		t.Fatalf("no error recorded for a failed pull")
	}

	src.err = nil
	src.body = seedXLSX(t)
	syncer.pullApp(ctx, defaultAppID, "test")

	lastPull, lastErr, failures := syncer.state.App(defaultAppID).snapshot()
	if lastErr != "" {
		t.Errorf("last_pull_error = %q, want cleared after success", lastErr)
	}
	if failures != 0 {
		t.Errorf("consecutive_failures = %d, want 0", failures)
	}
	if lastPull.IsZero() {
		t.Errorf("last_pull is zero after success")
	}
}

func TestPullAppContextCancelledIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	src := &stubSource{err: context.Canceled}
	syncer, _ := newTestSyncer(t, src)

	syncer.pullApp(ctx, defaultAppID, "test")

	if _, lastErr, failures := syncer.state.App(defaultAppID).snapshot(); lastErr != "" || failures != 0 {
		t.Errorf("shutdown recorded as a pull failure: err=%q failures=%d", lastErr, failures)
	}
}

func TestPullAppWritesCacheAndLoadFromCacheRestoresIt(t *testing.T) {
	ctx := context.Background()
	src := &stubSource{body: seedXLSX(t)}
	syncer, registry := newTestSyncer(t, src)

	syncer.pullApp(ctx, defaultAppID, "test")
	holder, _ := registry.Holder(defaultAppID)
	want := holder.Load()

	// A fresh process over the same cache dir must serve without upstream.
	restored := NewRegistry(syncer.cfg.AppIDs())
	restart := NewSyncer(syncer.cfg, restored, syncer.cache, NewState(syncer.cfg.AppIDs(), time.Now()),
		alwaysPrimary{identity: "test-pod"}, discardLogger())
	restart.LoadFromCache()

	got, _ := restored.Holder(defaultAppID)
	if got.Load() == nil {
		t.Fatalf("cache did not restore a snapshot")
	}
	if got.Load().Hash != want.Hash {
		t.Errorf("restored hash = %q, want %q", got.Load().Hash, want.Hash)
	}
	if got.Load().RowCount != want.RowCount {
		t.Errorf("restored rows = %d, want %d", got.Load().RowCount, want.RowCount)
	}
}

func TestLoadFromCacheRejectsExpiredEntry(t *testing.T) {
	src := &stubSource{body: seedXLSX(t)}
	syncer, _ := newTestSyncer(t, src)
	syncer.pullApp(context.Background(), defaultAppID, "test")

	// Beyond MAX_CACHE_AGE a restarted pod must hydrate afresh rather than
	// quietly serve content the rest of the fleet has moved past.
	syncer.cache.maxAge = time.Nanosecond

	restored := NewRegistry(syncer.cfg.AppIDs())
	restart := NewSyncer(syncer.cfg, restored, syncer.cache, NewState(syncer.cfg.AppIDs(), time.Now()),
		alwaysPrimary{identity: "test-pod"}, discardLogger())
	restart.LoadFromCache()

	h, _ := restored.Holder(defaultAppID)
	if h.Load() != nil {
		t.Errorf("expired cache entry was loaded")
	}
}
