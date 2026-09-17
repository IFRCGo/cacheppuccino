package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const seedHash = "51b23fc6f6c1de1c69a9b0f0f8a06f21c2e4a89f3a2e2b9d7c6a5e4d3c2b1a09"

type apiError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details"`
}

type responseEnvelope struct {
	Ok    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *apiError       `json:"error"`
}

type stringsData struct {
	App     string                       `json:"app"`
	Lang    string                       `json:"lang"`
	Pages   []string                     `json:"pages"`
	Strings map[string]map[string]string `json:"strings"`
}

// seedFrench installs a small fixture of French strings and returns the
// hash used as the ETag source.
func seedFrench(t *testing.T, srv *Server) string {
	t.Helper()

	return seedSnapshot(t, srv, seedHash, []StringRow{
		{Page: "a", Key: "hello", Lang: "fr", Value: "bonjour", UpdatedAt: time.Now()},
		{Page: "a", Key: "welcome", Lang: "fr", Value: "bienvenue", UpdatedAt: time.Now()},
		{Page: "b", Key: "bye", Lang: "fr", Value: "au revoir", UpdatedAt: time.Now()},
	})
}

func doGet(t *testing.T, h http.Handler, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) responseEnvelope {
	t.Helper()

	var env responseEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body: %q)", err, rec.Body.String())
	}
	return env
}

func decodeData(t *testing.T, env responseEnvelope, out any) {
	t.Helper()

	if env.Data == nil {
		t.Fatalf("envelope has no data field")
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		t.Fatalf("decode data: %v (data: %q)", err, string(env.Data))
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	rec := doGet(t, srv.routes(), "/healthz", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got, want := strings.TrimSpace(rec.Body.String()), `{"ok":true,"data":{"status":"ok"}}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestReadyz(t *testing.T) {
	srv := newTestServer(t)
	h := srv.routes()

	// Readiness reads the same snapshots /strings reads, so it cannot pass
	// while the API would fail for want of data.
	rec := doGet(t, h, "/readyz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no data: status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	env := decodeEnvelope(t, rec)
	if env.Ok {
		t.Errorf("no data: ok = true, want false")
	}
	if env.Error == nil || env.Error.Code != "no_data" {
		t.Errorf("no data: error = %+v, want code no_data", env.Error)
	}

	seedFrench(t, srv)

	rec = doGet(t, h, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("with data: status = %d, want %d", rec.Code, http.StatusOK)
	}
	var data struct {
		Status string `json:"status"`
	}
	decodeData(t, decodeEnvelope(t, rec), &data)
	if data.Status != "ready" {
		t.Errorf("status = %q, want %q", data.Status, "ready")
	}
}

func TestHealthzIgnoresMissingData(t *testing.T) {
	srv := newTestServer(t)

	// Liveness must not depend on data: restarting cannot conjure an
	// upstream, and failing here would turn a data problem into a restart
	// loop.
	rec := doGet(t, srv.routes(), "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHealthzFailsOnStalledSync(t *testing.T) {
	srv := newTestServer(t)
	seedFrench(t, srv)

	// A sync loop that has stopped ticking is the one condition a restart
	// actually fixes.
	srv.now = func() time.Time {
		return time.Now().Add(4 * srv.cfg.PullInterval)
	}

	rec := doGet(t, srv.routes(), "/healthz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	env := decodeEnvelope(t, rec)
	if env.Error == nil || env.Error.Code != "sync_stalled" {
		t.Errorf("error = %+v, want code sync_stalled", env.Error)
	}
}

func TestStatus(t *testing.T) {
	srv := newTestServer(t)
	h := srv.routes()

	rec := doGet(t, h, "/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	env := decodeEnvelope(t, rec)
	if !env.Ok {
		t.Fatalf("ok = false, want true")
	}

	var fields map[string]any
	decodeData(t, env, &fields)
	for _, k := range []string{"pod", "version", "primary", "ready", "apps"} {
		if _, present := fields[k]; !present {
			t.Errorf("data missing field %q", k)
		}
	}

	var data PodStatus
	decodeData(t, env, &data)
	if data.Version != "dev" {
		t.Errorf("version = %q, want %q", data.Version, "dev")
	}
	if data.Pod != "test-pod" {
		t.Errorf("pod = %q, want %q", data.Pod, "test-pod")
	}
	if data.Ready {
		t.Errorf("ready = true, want false before any import")
	}
	if len(data.Apps) != 1 {
		t.Fatalf("len(apps) = %d, want 1", len(data.Apps))
	}
	if data.Apps[0].Hash != "" {
		t.Errorf("apps[0].hash = %q, want empty before import", data.Apps[0].Hash)
	}

	hash := seedFrench(t, srv)

	rec = doGet(t, h, "/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("after import: status = %d, want %d", rec.Code, http.StatusOK)
	}
	decodeData(t, decodeEnvelope(t, rec), &data)
	if !data.Ready {
		t.Errorf("ready = false, want true after import")
	}
	if len(data.Apps) != 1 {
		t.Fatalf("after import: len(apps) = %d, want 1", len(data.Apps))
	}
	app := data.Apps[0]
	if app.App != defaultAppID {
		t.Errorf("apps[0].app = %q, want %q", app.App, defaultAppID)
	}
	if app.Hash != hash {
		t.Errorf("apps[0].hash = %q, want %q", app.Hash, hash)
	}
	if app.Rows != 3 {
		t.Errorf("apps[0].rows = %d, want 3", app.Rows)
	}
	if app.LastPull == "" {
		t.Errorf("apps[0].last_pull is empty after import")
	}
	if !app.Servable {
		t.Errorf("apps[0].servable = false, want true")
	}
}

func TestGetStringsValidation(t *testing.T) {
	srv := newTestServer(t)
	h := srv.routes()

	tests := []struct {
		name        string
		target      string
		wantDetails map[string]string
	}{
		{
			name:        "missing lang",
			target:      "/strings",
			wantDetails: map[string]string{"lang": "required"},
		},
		{
			name:        "missing pages",
			target:      "/strings?lang=fr",
			wantDetails: map[string]string{"page": "repeatable", "pages": "csv"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doGet(t, h, tc.target, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			env := decodeEnvelope(t, rec)
			if env.Ok {
				t.Errorf("ok = true, want false")
			}
			if env.Error == nil {
				t.Fatalf("error is nil")
			}
			if env.Error.Code != "bad_request" {
				t.Errorf("error.code = %q, want %q", env.Error.Code, "bad_request")
			}
			if !reflect.DeepEqual(env.Error.Details, tc.wantDetails) {
				t.Errorf("error.details = %v, want %v", env.Error.Details, tc.wantDetails)
			}
		})
	}
}

func TestGetStringsLangNormalization(t *testing.T) {
	srv := newTestServer(t)
	seedFrench(t, srv)

	rec := doGet(t, srv.routes(), "/strings?lang=FR&page=a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var data stringsData
	decodeData(t, decodeEnvelope(t, rec), &data)
	if data.Lang != "fr" {
		t.Errorf("lang = %q, want %q", data.Lang, "fr")
	}
	if got := data.Strings["a"]["hello"]; got != "bonjour" {
		t.Errorf("strings.a.hello = %q, want %q", got, "bonjour")
	}
}

func TestGetStringsPagesParsing(t *testing.T) {
	srv := newTestServer(t)
	seedFrench(t, srv)
	h := srv.routes()

	tests := []struct {
		name      string
		query     string
		wantPages []string
	}{
		{"repeated page params", "page=a&page=b", []string{"a", "b"}},
		{"csv pages", "pages=a,b", []string{"a", "b"}},
		{"page wins over pages", "page=a&pages=b", []string{"a"}},
		{"duplicate page params deduped", "page=a&page=a&page=b", []string{"a", "b"}},
		{"csv duplicates deduped", "pages=a,a,b", []string{"a", "b"}},
		{"csv whitespace trimmed", "pages=%20a%20,%20b%20", []string{"a", "b"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doGet(t, h, "/strings?lang=fr&"+tc.query, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
			}

			var data stringsData
			decodeData(t, decodeEnvelope(t, rec), &data)
			if !reflect.DeepEqual(data.Pages, tc.wantPages) {
				t.Errorf("pages = %v, want %v", data.Pages, tc.wantPages)
			}
			if len(data.Strings) != len(tc.wantPages) {
				t.Errorf("strings has %d pages, want %d", len(data.Strings), len(tc.wantPages))
			}
			for _, p := range tc.wantPages {
				if _, present := data.Strings[p]; !present {
					t.Errorf("strings missing page %q", p)
				}
			}
		})
	}
}

func TestGetStringsUnknownPage(t *testing.T) {
	srv := newTestServer(t)
	seedFrench(t, srv)

	rec := doGet(t, srv.routes(), "/strings?lang=fr&page=a&page=nope", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var data stringsData
	decodeData(t, decodeEnvelope(t, rec), &data)
	unknown, present := data.Strings["nope"]
	if !present {
		t.Fatalf("strings missing unknown page %q", "nope")
	}
	if len(unknown) != 0 {
		t.Errorf("strings.nope = %v, want empty object", unknown)
	}
	if got := data.Strings["a"]["hello"]; got != "bonjour" {
		t.Errorf("strings.a.hello = %q, want %q", got, "bonjour")
	}
}

func TestGetStringsWithoutDataIsUnavailable(t *testing.T) {
	srv := newTestServer(t)

	// Serving an empty 200 would look like "this page has no translations"
	// to a client that cannot tell the difference.
	rec := doGet(t, srv.routes(), "/strings?lang=fr&page=a", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	env := decodeEnvelope(t, rec)
	if env.Error == nil || env.Error.Code != "no_data" {
		t.Errorf("error = %+v, want code no_data", env.Error)
	}
	if etag := rec.Header().Get("ETag"); etag != "" {
		t.Errorf("ETag = %q, want none without data", etag)
	}
}

func TestGetStringsUnknownApp(t *testing.T) {
	srv := newTestServer(t)
	seedFrench(t, srv)

	rec := doGet(t, srv.routes(), "/strings?lang=fr&page=a&app=nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	env := decodeEnvelope(t, rec)
	if env.Error == nil || env.Error.Code != "unknown_app" {
		t.Errorf("error = %+v, want code unknown_app", env.Error)
	}
}

func TestGetStringsETag(t *testing.T) {
	srv := newTestServer(t)
	hash := seedFrench(t, srv)
	h := srv.routes()
	etag := `"` + hash + `"`

	tests := []struct {
		name        string
		ifNoneMatch string
		wantStatus  int
	}{
		{"no conditional header", "", http.StatusOK},
		{"matching etag", etag, http.StatusNotModified},
		{"star", "*", http.StatusNotModified},
		{"stale etag", `"stale"`, http.StatusOK},
		{"multiple candidates with match", `"stale", ` + etag, http.StatusNotModified},
		{"weak matching etag", "W/" + etag, http.StatusNotModified},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			header := map[string]string{}
			if tc.ifNoneMatch != "" {
				header["If-None-Match"] = tc.ifNoneMatch
			}
			rec := doGet(t, h, "/strings?lang=fr&page=a", header)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			// Both 200 and 304 must carry the caching headers.
			if got := rec.Header().Get("ETag"); got != etag {
				t.Errorf("ETag = %q, want %q", got, etag)
			}
			if got, want := rec.Header().Get("Cache-Control"), "public, max-age=60"; got != want {
				t.Errorf("Cache-Control = %q, want %q", got, want)
			}

			if tc.wantStatus == http.StatusNotModified {
				if rec.Body.Len() != 0 {
					t.Errorf("304 body = %q, want empty", rec.Body.String())
				}
				return
			}

			var data stringsData
			decodeData(t, decodeEnvelope(t, rec), &data)
			if got := data.Strings["a"]["hello"]; got != "bonjour" {
				t.Errorf("strings.a.hello = %q, want %q", got, "bonjour")
			}
		})
	}
}
