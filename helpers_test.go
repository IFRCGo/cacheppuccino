package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

// discardLogger returns a logger that swallows all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// testConfig returns a config with the defaults a unit test needs: url
// source (no credentials), a cache under t.TempDir(), election off.
func testConfig(t *testing.T) Config {
	t.Helper()

	t.Setenv("TRANSLATION_SOURCE", sourceURL)
	t.Setenv("TRANSLATION_XLSX_URL", "https://example.test/translations.xlsx")
	t.Setenv("CACHE_DIR", t.TempDir())
	t.Setenv("POD_NAME", "test-pod")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// newTestServer builds a server with no data loaded.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	cfg := testConfig(t)
	registry := NewRegistry(cfg.AppIDs())
	state := NewState(cfg.AppIDs(), time.Now())
	elector := Elector(alwaysPrimary{identity: cfg.PodName})
	syncer := NewSyncer(cfg, registry, NewCache(cfg.CacheDir, cfg.MaxCacheAge), state, elector, discardLogger())

	return NewServer(cfg, registry, state, syncer, elector, discardLogger())
}

// seedSnapshot installs a snapshot for the default application and returns
// its hash, which is also the ETag source.
func seedSnapshot(t *testing.T, srv *Server, hash string, rows []StringRow) string {
	t.Helper()

	h, ok := srv.registry.Holder(defaultAppID)
	if !ok {
		t.Fatalf("no holder for %q", defaultAppID)
	}
	snap := NewSnapshot(defaultAppID, hash, time.Now(), rows)
	if !snap.Servable() {
		t.Fatalf("seed snapshot is not servable")
	}
	h.Store(snap)
	srv.state.App(defaultAppID).recordSuccess(time.Now())
	return hash
}

// xlsxSheet describes one sheet of an in-memory test workbook.
type xlsxSheet struct {
	name string
	rows [][]any
}

// buildXLSX builds an in-memory workbook. The first sheet replaces the
// default "Sheet1"; subsequent sheets are appended.
func buildXLSX(t *testing.T, sheets []xlsxSheet) []byte {
	t.Helper()

	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	for i, sheet := range sheets {
		if i == 0 {
			if sheet.name != "Sheet1" {
				if err := f.SetSheetName("Sheet1", sheet.name); err != nil {
					t.Fatalf("SetSheetName: %v", err)
				}
			}
		} else {
			if _, err := f.NewSheet(sheet.name); err != nil {
				t.Fatalf("NewSheet: %v", err)
			}
		}
		for rowIndex, row := range sheet.rows {
			cell, err := excelize.CoordinatesToCellName(1, rowIndex+1)
			if err != nil {
				t.Fatalf("CoordinatesToCellName: %v", err)
			}
			if err := f.SetSheetRow(sheet.name, cell, &row); err != nil {
				t.Fatalf("SetSheetRow: %v", err)
			}
		}
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatalf("WriteToBuffer: %v", err)
	}
	return buf.Bytes()
}

// translationsXLSX builds a workbook with a single "Translations" sheet;
// rows[0] is the header.
func translationsXLSX(t *testing.T, rows [][]string) []byte {
	t.Helper()

	anyRows := make([][]any, 0, len(rows))
	for _, row := range rows {
		anyRow := make([]any, len(row))
		for i, cell := range row {
			anyRow[i] = cell
		}
		anyRows = append(anyRows, anyRow)
	}
	return buildXLSX(t, []xlsxSheet{{name: "Translations", rows: anyRows}})
}

// stubSource stands in for upstream. Mutating body or err between calls is
// how tests drive a change, an outage, or a bad file.
type stubSource struct {
	mu    sync.Mutex
	body  []byte
	err   error
	calls int
}

func (s *stubSource) Name() string { return "stub" }

func (s *stubSource) Fetch(ctx context.Context, _ *slog.Logger) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.body, nil
}

func (s *stubSource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newTestSyncer wires a Syncer to a stub upstream and a cache under
// t.TempDir().
func newTestSyncer(t *testing.T, src XLSXSource) (*Syncer, *Registry) {
	t.Helper()

	cfg := testConfig(t)
	registry := NewRegistry(cfg.AppIDs())
	state := NewState(cfg.AppIDs(), time.Now())
	cache := NewCache(cfg.CacheDir, cfg.MaxCacheAge)

	syncer := NewSyncer(cfg, registry, cache, state, alwaysPrimary{identity: cfg.PodName}, discardLogger())
	syncer.sources[defaultAppID] = src
	return syncer, registry
}

// toPlain copies a snapshot's shared inner maps so a test can compare them
// without holding references into the snapshot.
func toPlain(in map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(in))
	for page, kv := range in {
		copied := make(map[string]string, len(kv))
		for k, v := range kv {
			copied[k] = v
		}
		out[page] = copied
	}
	return out
}
