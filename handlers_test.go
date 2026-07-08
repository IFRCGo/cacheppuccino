package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const htTestHash = "51b23fc6f6c1de1c69a9b0f0f8a06f21c2e4a89f3a2e2b9d7c6a5e4d3c2b1a09"

type htAPIError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details"`
}

type htEnvelope struct {
	Ok    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *htAPIError     `json:"error"`
}

type htStringsData struct {
	Lang    string                       `json:"lang"`
	Pages   []string                     `json:"pages"`
	Strings map[string]map[string]string `json:"strings"`
}

func htNewServer(t *testing.T) *Server {
	t.Helper()

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return &Server{
		db:     db,
		ready:  &ReadyState{},
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

// htSeedFrench imports a small fixture of French strings and returns the
// import hash used as the ETag source.
func htSeedFrench(t *testing.T, srv *Server) string {
	t.Helper()

	rows := []StringRow{
		{Page: "a", Key: "hello", Lang: "fr", Value: "bonjour", UpdatedAt: time.Now()},
		{Page: "a", Key: "welcome", Lang: "fr", Value: "bienvenue", UpdatedAt: time.Now()},
		{Page: "b", Key: "bye", Lang: "fr", Value: "au revoir", UpdatedAt: time.Now()},
	}
	if err := srv.db.ReplaceImport(context.Background(), rows, htTestHash, time.Now()); err != nil {
		t.Fatalf("ReplaceImport: %v", err)
	}
	return htTestHash
}

func htGet(t *testing.T, h http.Handler, target string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func htDecodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) htEnvelope {
	t.Helper()

	var env htEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body: %q)", err, rec.Body.String())
	}
	return env
}

func htDecodeData(t *testing.T, env htEnvelope, out any) {
	t.Helper()

	if env.Data == nil {
		t.Fatalf("envelope has no data field")
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		t.Fatalf("decode data: %v (data: %q)", err, string(env.Data))
	}
}

func TestHealthz(t *testing.T) {
	srv := htNewServer(t)
	rec := htGet(t, srv.routes(), "/healthz", nil)

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
	srv := htNewServer(t)
	h := srv.routes()

	// Always 200 while the process is up, even before any servable data
	// exists; the servable-data signal is `ready` on /status.
	for _, ready := range []bool{false, true} {
		srv.ready.SetReady(ready)

		rec := htGet(t, h, "/readyz", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("ready=%v: status = %d, want %d", ready, rec.Code, http.StatusOK)
		}
		env := htDecodeEnvelope(t, rec)
		if !env.Ok {
			t.Errorf("ready=%v: ok = false, want true", ready)
		}
		var data struct {
			Status string `json:"status"`
		}
		htDecodeData(t, env, &data)
		if data.Status != "ready" {
			t.Errorf("ready=%v: status = %q, want %q", ready, data.Status, "ready")
		}
	}
}

func TestStatus(t *testing.T) {
	srv := htNewServer(t)
	h := srv.routes()

	rec := htGet(t, h, "/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	env := htDecodeEnvelope(t, rec)
	if !env.Ok {
		t.Fatalf("ok = false, want true")
	}

	var fields map[string]any
	htDecodeData(t, env, &fields)
	for _, k := range []string{"last_pull", "last_hash", "ready", "version"} {
		if _, present := fields[k]; !present {
			t.Errorf("data missing field %q", k)
		}
	}

	var data StatusResponse
	htDecodeData(t, env, &data)
	if data.Version != "dev" {
		t.Errorf("version = %q, want %q", data.Version, "dev")
	}
	if data.Ready {
		t.Errorf("ready = true, want false before SetReady")
	}
	if data.LastHash != "" {
		t.Errorf("last_hash = %q, want empty before import", data.LastHash)
	}

	hash := htSeedFrench(t, srv)
	srv.ready.SetReady(true)

	rec = htGet(t, h, "/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("after import: status = %d, want %d", rec.Code, http.StatusOK)
	}
	htDecodeData(t, htDecodeEnvelope(t, rec), &data)
	if data.LastHash != hash {
		t.Errorf("last_hash = %q, want %q", data.LastHash, hash)
	}
	if data.LastPull == "" {
		t.Errorf("last_pull is empty after import")
	}
	if !data.Ready {
		t.Errorf("ready = false, want true after SetReady")
	}
}

func TestGetStringsValidation(t *testing.T) {
	srv := htNewServer(t)
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
			rec := htGet(t, h, tc.target, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			env := htDecodeEnvelope(t, rec)
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
	srv := htNewServer(t)
	htSeedFrench(t, srv)

	rec := htGet(t, srv.routes(), "/strings?lang=FR&page=a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var data htStringsData
	htDecodeData(t, htDecodeEnvelope(t, rec), &data)
	if data.Lang != "fr" {
		t.Errorf("lang = %q, want %q", data.Lang, "fr")
	}
	if got := data.Strings["a"]["hello"]; got != "bonjour" {
		t.Errorf("strings.a.hello = %q, want %q", got, "bonjour")
	}
}

func TestGetStringsPagesParsing(t *testing.T) {
	srv := htNewServer(t)
	htSeedFrench(t, srv)
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
			rec := htGet(t, h, "/strings?lang=fr&"+tc.query, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
			}

			var data htStringsData
			htDecodeData(t, htDecodeEnvelope(t, rec), &data)
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
	srv := htNewServer(t)
	htSeedFrench(t, srv)

	rec := htGet(t, srv.routes(), "/strings?lang=fr&page=a&page=nope", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var data htStringsData
	htDecodeData(t, htDecodeEnvelope(t, rec), &data)
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

func TestGetStringsNoETagBeforeImport(t *testing.T) {
	srv := htNewServer(t)

	rec := htGet(t, srv.routes(), "/strings?lang=fr&page=a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if etag := rec.Header().Get("ETag"); etag != "" {
		t.Errorf("ETag = %q, want no ETag on fresh db", etag)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q, want none on fresh db", cc)
	}
}

func TestGetStringsETag(t *testing.T) {
	srv := htNewServer(t)
	hash := htSeedFrench(t, srv)
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
			rec := htGet(t, h, "/strings?lang=fr&page=a", header)

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

			var data htStringsData
			htDecodeData(t, htDecodeEnvelope(t, rec), &data)
			if got := data.Strings["a"]["hello"]; got != "bonjour" {
				t.Errorf("strings.a.hello = %q, want %q", got, "bonjour")
			}
		})
	}
}
