package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMeasureRealExport sizes the production export so pod resource limits
// and the peer transfer format are set from a measurement rather than a
// guess. Skipped unless MEASURE_REAL=1; never run in CI.
func TestMeasureRealExport(t *testing.T) {
	if os.Getenv("MEASURE_REAL") != "1" {
		t.Skip("set MEASURE_REAL=1 to measure against the real translation API")
	}

	app := AppConfig{
		Source:        sourceAPI,
		BaseURL:       os.Getenv("TRANSLATION_BASE_URL"),
		ApplicationID: os.Getenv("TRANSLATION_APPLICATION_ID"),
		APIKey:        os.Getenv("TRANSLATION_API_KEY"),
	}
	src := NewAPISource(app, 120*time.Second)

	var m0, m1, m2, m3 runtime.MemStats
	settle := func(m *runtime.MemStats) {
		runtime.GC()
		runtime.ReadMemStats(m)
	}

	settle(&m0)

	start := time.Now()
	xlsx, err := src.Fetch(context.Background(), discardLogger())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	fetchDur := time.Since(start)
	settle(&m1)

	start = time.Now()
	rows, err := ParseXLSX(xlsx)
	if err != nil {
		t.Fatalf("ParseXLSX: %v", err)
	}
	parseDur := time.Since(start)
	settle(&m2)

	start = time.Now()
	snap := NewSnapshot(defaultAppID, HashBytes(xlsx), time.Now(), rows, xlsx)
	buildDur := time.Since(start)
	settle(&m3)

	langs, pages := map[string]struct{}{}, map[string]struct{}{}
	for _, r := range rows {
		langs[r.Lang] = struct{}{}
		pages[r.Page] = struct{}{}
	}

	// Drop the intermediates an import discards, leaving the steady-state cost.
	rows = nil
	var mSteady runtime.MemStats
	settle(&mSteady)
	runtime.KeepAlive(snap)

	mib := func(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }

	t.Logf("")
	t.Logf("  XLSX download      %s in %s", mib(uint64(len(xlsx))), fetchDur.Round(time.Millisecond))
	t.Logf("  parse              %s", parseDur.Round(time.Millisecond))
	t.Logf("  snapshot build     %s", buildDur.Round(time.Millisecond))
	t.Logf("")
	t.Logf("  pages              %d", len(pages))
	t.Logf("  languages          %d", len(langs))
	t.Logf("  rows (page x key x lang)  %d", snap.RowCount)
	t.Logf("")
	t.Logf("  heap after fetch   %s", mib(m1.HeapAlloc-m0.HeapAlloc))
	t.Logf("  heap after parse   %s", mib(m2.HeapAlloc-m0.HeapAlloc))
	t.Logf("  heap after build   %s", mib(m3.HeapAlloc-m0.HeapAlloc))
	t.Logf("  heap steady-state  %s  <- one live snapshot", mib(mSteady.HeapAlloc-m0.HeapAlloc))
	t.Logf("  peak RSS (VmHWM)   %s", vmHWM())
	t.Logf("")
	t.Logf("  a swap holds two snapshots: ~%s", mib(2*(mSteady.HeapAlloc-m0.HeapAlloc)))
}

// vmHWM reports the process high-water RSS, which is what a container limit
// is actually compared against.
func vmHWM() string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "unavailable"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 {
			if kb, err := strconv.Atoi(f[1]); err == nil {
				return fmt.Sprintf("%.1f MiB", float64(kb)/1024)
			}
		}
	}
	return "unavailable"
}
