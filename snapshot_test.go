package main

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func testRows() []StringRow {
	return []StringRow{
		{Page: "home", Key: "greeting", Lang: "en", Value: "Hello"},
		{Page: "home", Key: "greeting", Lang: "fr", Value: "Bonjour"},
		{Page: "about", Key: "title", Lang: "en", Value: "About"},
	}
}

func TestSnapshotGet(t *testing.T) {
	snap := NewSnapshot(defaultAppID, "h1", time.Now(), testRows())

	tests := []struct {
		name      string
		pages     []string
		lang      string
		wantPages []string
		want      map[string]map[string]string
	}{
		{
			name:      "known pages",
			pages:     []string{"home", "about"},
			lang:      "en",
			wantPages: []string{"home", "about"},
			want: map[string]map[string]string{
				"home":  {"greeting": "Hello"},
				"about": {"title": "About"},
			},
		},
		{
			// An unknown page yields an empty object so the response shape
			// does not depend on which pages happen to exist.
			name:      "unknown page",
			pages:     []string{"home", "nope"},
			lang:      "en",
			wantPages: []string{"home", "nope"},
			want: map[string]map[string]string{
				"home": {"greeting": "Hello"},
				"nope": {},
			},
		},
		{
			name:      "known page without the language",
			pages:     []string{"about"},
			lang:      "fr",
			wantPages: []string{"about"},
			want:      map[string]map[string]string{"about": {}},
		},
		{
			name:      "dedupes and trims preserving order",
			pages:     []string{" home ", "about", "home", "", "  "},
			lang:      "en",
			wantPages: []string{"home", "about"},
			want: map[string]map[string]string{
				"home":  {"greeting": "Hello"},
				"about": {"title": "About"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, pages := snap.Get(tc.pages, tc.lang)
			if !reflect.DeepEqual(pages, tc.wantPages) {
				t.Errorf("pages = %v, want %v", pages, tc.wantPages)
			}
			if !reflect.DeepEqual(toPlain(got), tc.want) {
				t.Errorf("strings = %v, want %v", toPlain(got), tc.want)
			}
		})
	}
}

func TestSnapshotDuplicateRowsLastOneWins(t *testing.T) {
	snap := NewSnapshot(defaultAppID, "h1", time.Now(), []StringRow{
		{Page: "home", Key: "greeting", Lang: "en", Value: "first"},
		{Page: "home", Key: "greeting", Lang: "en", Value: "second"},
	})

	got, _ := snap.Get([]string{"home"}, "en")
	if got["home"]["greeting"] != "second" {
		t.Errorf("greeting = %q, want %q", got["home"]["greeting"], "second")
	}
}

func TestSnapshotServable(t *testing.T) {
	tests := []struct {
		name string
		snap *Snapshot
		want bool
	}{
		{"nil", nil, false},
		{"no rows", NewSnapshot(defaultAppID, "h", time.Now(), nil), false},
		{"with rows", NewSnapshot(defaultAppID, "h", time.Now(), testRows()), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snap.Servable(); got != tc.want {
				t.Errorf("Servable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The probe pair must be identical on every pod holding the same content,
// so readiness means the same thing fleet-wide.
func TestSnapshotProbeIsDeterministic(t *testing.T) {
	a := NewSnapshot(defaultAppID, "h", time.Now(), testRows())

	shuffled := []StringRow{testRows()[2], testRows()[0], testRows()[1]}
	b := NewSnapshot(defaultAppID, "h", time.Now(), shuffled)

	if a.probePage != b.probePage || a.probeLang != b.probeLang {
		t.Errorf("probe = (%q,%q) and (%q,%q), want identical",
			a.probePage, a.probeLang, b.probePage, b.probeLang)
	}
}

func TestHolderStoreIfNewer(t *testing.T) {
	base := time.Now()
	h := &Holder{appID: defaultAppID}

	older := NewSnapshot(defaultAppID, "old", base, testRows())
	newer := NewSnapshot(defaultAppID, "new", base.Add(time.Minute), testRows())

	if !h.StoreIfNewer(newer) {
		t.Fatalf("first store rejected")
	}

	// A lagging peer must never be able to walk this pod backwards to
	// content it has already stopped serving.
	if h.StoreIfNewer(older) {
		t.Errorf("adopted an older snapshot")
	}
	if h.Load().Hash != "new" {
		t.Errorf("hash = %q, want %q", h.Load().Hash, "new")
	}

	// Same content at a later timestamp is not a change worth swapping for:
	// the ETag would not move and clients would re-download for nothing.
	same := NewSnapshot(defaultAppID, "new", base.Add(time.Hour), testRows())
	if h.StoreIfNewer(same) {
		t.Errorf("adopted a snapshot with an unchanged hash")
	}
}

func TestHolderStoreIfNewerIsRaceFree(t *testing.T) {
	base := time.Now()
	h := &Holder{appID: defaultAppID}

	var wg sync.WaitGroup
	for i := 1; i <= 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			h.StoreIfNewer(NewSnapshot(defaultAppID, string(rune('a'+n)), base.Add(time.Duration(n)*time.Second), testRows()))
		}(i)
	}
	wg.Wait()

	// Whichever writers raced, the winner must be the newest offered.
	if got := h.Load().Version(); got != base.Add(32*time.Second).UnixMilli() {
		t.Errorf("version = %d, want the newest offered", got)
	}
}

func TestRegistryServableNeedsOnlyOneApp(t *testing.T) {
	r := NewRegistry([]string{"a", "b"})
	if r.Servable() {
		t.Fatalf("empty registry reported servable")
	}

	// One broken application must not make the pod unready for the healthy
	// one; /strings answers 503 per application instead.
	h, _ := r.Holder("a")
	h.Store(NewSnapshot("a", "h", time.Now(), testRows()))

	if !r.Servable() {
		t.Errorf("registry with one loaded app reported not servable")
	}
}
