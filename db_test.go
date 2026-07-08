package main

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

var fixedUpdatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newStringRow(page, key, lang, value string) StringRow {
	return StringRow{Page: page, Key: key, Lang: lang, Value: value, UpdatedAt: fixedUpdatedAt}
}

func mustImport(t *testing.T, db *DB, rows []StringRow, hash string, pulledAt time.Time) {
	t.Helper()
	if err := db.ReplaceImport(context.Background(), rows, hash, pulledAt); err != nil {
		t.Fatalf("ReplaceImport: %v", err)
	}
}

func countStrings(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM strings").Scan(&n); err != nil {
		t.Fatalf("count strings: %v", err)
	}
	return n
}

// mustMeta fetches a meta key and fails the test if the key is absent.
func mustMeta(t *testing.T, db *DB, key string) string {
	t.Helper()
	v, ok, err := db.GetMeta(context.Background(), key)
	if err != nil {
		t.Fatalf("GetMeta(%q): %v", key, err)
	}
	if !ok {
		t.Fatalf("GetMeta(%q): key missing", key)
	}
	return v
}

// lastPullTime parses the stored last-pull meta value as RFC3339.
func lastPullTime(t *testing.T, db *DB) time.Time {
	t.Helper()
	raw := mustMeta(t, db, metaKeyLastPull)
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("meta %q = %q is not RFC3339: %v", metaKeyLastPull, raw, err)
	}
	return parsed
}

func TestOpenDBCreatesParentDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "test.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB with non-existent parent dirs: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.HasStrings(context.Background()); err != nil {
		t.Fatalf("HasStrings on freshly created DB: %v", err)
	}
}

func TestOpenDBEnablesWAL(t *testing.T) {
	db := openTestDB(t)

	var mode string
	if err := db.sql.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestHasStrings(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	has, err := db.HasStrings(ctx)
	if err != nil {
		t.Fatalf("HasStrings on fresh DB: %v", err)
	}
	if has {
		t.Fatal("HasStrings on fresh DB = true, want false")
	}

	mustImport(t, db, []StringRow{newStringRow("home", "title", "en", "Home")}, "h1", time.Now())

	has, err = db.HasStrings(ctx)
	if err != nil {
		t.Fatalf("HasStrings after import: %v", err)
	}
	if !has {
		t.Fatal("HasStrings after import = false, want true")
	}
}

func TestReplaceImportBasics(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	hash := "deadbeefcafe"
	pulledAt := time.Date(2026, 7, 8, 10, 30, 0, 0, time.FixedZone("NPT", 5*3600+45*60))

	rows := []StringRow{
		newStringRow("home", "title", "en", "Home"),
		newStringRow("home", "subtitle", "en", "Welcome"),
		newStringRow("about", "title", "en", "About"),
	}
	mustImport(t, db, rows, hash, pulledAt)

	got, cleaned, err := db.GetStringsByPagesLang(ctx, []string{"home", "about"}, "en")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang: %v", err)
	}
	want := map[string]map[string]string{
		"home":  {"title": "Home", "subtitle": "Welcome"},
		"about": {"title": "About"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetStringsByPagesLang map = %v, want %v", got, want)
	}
	if wantCleaned := []string{"home", "about"}; !slices.Equal(cleaned, wantCleaned) {
		t.Errorf("cleaned = %v, want %v", cleaned, wantCleaned)
	}

	if gotHash := mustMeta(t, db, metaKeyLastXLSXHash); gotHash != hash {
		t.Errorf("meta %q = %q, want %q", metaKeyLastXLSXHash, gotHash, hash)
	}
	if gotPull := lastPullTime(t, db); !gotPull.Equal(pulledAt) {
		t.Errorf("meta %q = %v, want time equal to %v", metaKeyLastPull, gotPull, pulledAt)
	}
}

func TestReplaceImportFullReplace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := []StringRow{
		newStringRow("old", "title", "en", "Old title"),
		newStringRow("old", "body", "en", "Old body"),
	}
	mustImport(t, db, first, "hash-1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	second := []StringRow{
		newStringRow("new", "title", "en", "New title"),
	}
	pulledAt2 := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	mustImport(t, db, second, "hash-2", pulledAt2)

	if n := countStrings(t, db); n != 1 {
		t.Errorf("row count after second import = %d, want 1", n)
	}

	got, _, err := db.GetStringsByPagesLang(ctx, []string{"old", "new"}, "en")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang: %v", err)
	}
	if len(got["old"]) != 0 {
		t.Errorf("stale rows survived import: got[\"old\"] = %v, want empty", got["old"])
	}
	if !reflect.DeepEqual(got["new"], map[string]string{"title": "New title"}) {
		t.Errorf("got[\"new\"] = %v, want map with only new title", got["new"])
	}

	if gotHash := mustMeta(t, db, metaKeyLastXLSXHash); gotHash != "hash-2" {
		t.Errorf("meta %q = %q, want %q", metaKeyLastXLSXHash, gotHash, "hash-2")
	}
	if gotPull := lastPullTime(t, db); !gotPull.Equal(pulledAt2) {
		t.Errorf("meta %q = %v, want time equal to %v", metaKeyLastPull, gotPull, pulledAt2)
	}
}

func TestReplaceImportDuplicateRowsLastWins(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	rows := []StringRow{
		newStringRow("home", "title", "en", "first"),
		newStringRow("home", "title", "en", "second"),
		newStringRow("home", "title", "en", "third"),
	}
	if err := db.ReplaceImport(ctx, rows, "h", time.Now()); err != nil {
		t.Fatalf("ReplaceImport with duplicate (page,key,lang) rows: %v", err)
	}

	if n := countStrings(t, db); n != 1 {
		t.Errorf("row count = %d, want 1", n)
	}

	got, _, err := db.GetStringsByPagesLang(ctx, []string{"home"}, "en")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang: %v", err)
	}
	if v := got["home"]["title"]; v != "third" {
		t.Errorf("value = %q, want %q (last duplicate wins)", v, "third")
	}
}

func TestReplaceImportChunking(t *testing.T) {
	db := openTestDB(t)

	// > 400 rows spans several ~189-row insert chunks.
	const total = 450
	rows := make([]StringRow, 0, total)
	for i := range total {
		rows = append(rows, newStringRow("bulk", fmt.Sprintf("key-%03d", i), "en", fmt.Sprintf("value-%03d", i)))
	}
	mustImport(t, db, rows, "bulk-hash", time.Now())

	if n := countStrings(t, db); n != total {
		t.Fatalf("row count = %d, want %d", n, total)
	}

	got, _, err := db.GetStringsByPagesLang(context.Background(), []string{"bulk"}, "en")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang: %v", err)
	}
	if len(got["bulk"]) != total {
		t.Errorf("len(got[\"bulk\"]) = %d, want %d", len(got["bulk"]), total)
	}
	if v := got["bulk"]["key-449"]; v != "value-449" {
		t.Errorf("got[\"bulk\"][\"key-449\"] = %q, want %q", v, "value-449")
	}
}

func TestReplaceImportEmptyWipesTableAndSetsMeta(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	mustImport(t, db, []StringRow{newStringRow("home", "title", "en", "Home")}, "hash-1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	pulledAt2 := time.Date(2026, 3, 3, 6, 7, 8, 0, time.UTC)
	if err := db.ReplaceImport(ctx, nil, "hash-empty", pulledAt2); err != nil {
		t.Fatalf("ReplaceImport with nil rows: %v", err)
	}

	has, err := db.HasStrings(ctx)
	if err != nil {
		t.Fatalf("HasStrings: %v", err)
	}
	if has {
		t.Error("HasStrings after empty import = true, want false")
	}

	if gotHash := mustMeta(t, db, metaKeyLastXLSXHash); gotHash != "hash-empty" {
		t.Errorf("meta %q = %q, want %q", metaKeyLastXLSXHash, gotHash, "hash-empty")
	}
	if gotPull := lastPullTime(t, db); !gotPull.Equal(pulledAt2) {
		t.Errorf("meta %q = %v, want time equal to %v", metaKeyLastPull, gotPull, pulledAt2)
	}
}

func TestGetStringsByPagesLangPageCleaning(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	mustImport(t, db, []StringRow{
		newStringRow("home", "title", "en", "Home"),
		newStringRow("about", "title", "en", "About"),
	}, "h", time.Now())

	cases := []struct {
		name        string
		pages       []string
		wantCleaned []string
	}{
		{"trim and dedupe", []string{" home ", "home", "\tabout\n", "home"}, []string{"home", "about"}},
		{"dedupe applies after trim", []string{"home", "  home"}, []string{"home"}},
		{"unknown page kept", []string{"home", "no-such-page"}, []string{"home", "no-such-page"}},
		{"only empty and whitespace", []string{"", "   ", "\t\n"}, []string{}},
		{"nil pages", nil, []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cleaned, err := db.GetStringsByPagesLang(ctx, tc.pages, "en")
			if err != nil {
				t.Fatalf("GetStringsByPagesLang(%v): %v", tc.pages, err)
			}
			if !slices.Equal(cleaned, tc.wantCleaned) {
				t.Errorf("cleaned = %v, want %v", cleaned, tc.wantCleaned)
			}
			if got == nil {
				t.Fatal("result map is nil, want non-nil")
			}
			if len(got) != len(tc.wantCleaned) {
				t.Errorf("len(map) = %d, want %d (one entry per cleaned page)", len(got), len(tc.wantCleaned))
			}
			for _, p := range tc.wantCleaned {
				m, ok := got[p]
				if !ok {
					t.Errorf("map missing requested page %q", p)
					continue
				}
				if m == nil {
					t.Errorf("map for page %q is nil, want non-nil", p)
				}
			}
		})
	}
}

func TestGetStringsByPagesLangUnknownPageEmptyMap(t *testing.T) {
	db := openTestDB(t)

	mustImport(t, db, []StringRow{newStringRow("home", "title", "en", "Home")}, "h", time.Now())

	got, _, err := db.GetStringsByPagesLang(context.Background(), []string{"missing"}, "en")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang: %v", err)
	}
	m, ok := got["missing"]
	if !ok {
		t.Fatal("unknown page absent from map, want present with empty map")
	}
	if m == nil {
		t.Fatal("map for unknown page is nil, want empty non-nil map")
	}
	if len(m) != 0 {
		t.Fatalf("map for unknown page = %v, want empty", m)
	}
}

func TestGetStringsByPagesLangFiltersByLang(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	mustImport(t, db, []StringRow{
		newStringRow("home", "title", "en", "Home"),
		newStringRow("home", "title", "fr", "Accueil"),
		newStringRow("home", "greeting", "fr", "Bonjour"),
	}, "h", time.Now())

	gotFR, _, err := db.GetStringsByPagesLang(ctx, []string{"home"}, "fr")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang(fr): %v", err)
	}
	wantFR := map[string]string{"title": "Accueil", "greeting": "Bonjour"}
	if !reflect.DeepEqual(gotFR["home"], wantFR) {
		t.Errorf("fr map = %v, want %v", gotFR["home"], wantFR)
	}

	gotEN, _, err := db.GetStringsByPagesLang(ctx, []string{"home"}, "en")
	if err != nil {
		t.Fatalf("GetStringsByPagesLang(en): %v", err)
	}
	wantEN := map[string]string{"title": "Home"}
	if !reflect.DeepEqual(gotEN["home"], wantEN) {
		t.Errorf("en map = %v, want %v", gotEN["home"], wantEN)
	}
}

func TestGetMetaMissingKey(t *testing.T) {
	db := openTestDB(t)

	v, ok, err := db.GetMeta(context.Background(), "no-such-key")
	if err != nil {
		t.Fatalf("GetMeta on missing key: %v", err)
	}
	if ok {
		t.Error("ok = true, want false")
	}
	if v != "" {
		t.Errorf("value = %q, want empty string", v)
	}
}

func TestSetMetaOverwrites(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.SetMeta(ctx, "k1", "v1"); err != nil {
		t.Fatalf("SetMeta initial: %v", err)
	}
	if err := db.SetMeta(ctx, "k1", "v2"); err != nil {
		t.Fatalf("SetMeta overwrite: %v", err)
	}

	v, ok, err := db.GetMeta(ctx, "k1")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if v != "v2" {
		t.Errorf("value = %q, want %q", v, "v2")
	}
}
