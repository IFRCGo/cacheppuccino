package main

import (
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// emptyStrings is returned for pages absent from a snapshot. Callers must
// treat every map reachable from a Snapshot as read-only.
var emptyStrings = map[string]string{}

// Snapshot is an immutable view of one application's translations, built
// once per import and never mutated. Handlers serve the inner maps directly
// rather than copying them, so nothing may write through a Snapshot.
type Snapshot struct {
	AppID      string
	Hash       string
	ImportedAt time.Time
	RowCount   int

	// byPage maps page -> lang -> key -> value.
	byPage map[string]map[string]map[string]string

	// probePage and probeLang name a (page, lang) pair known to hold at
	// least one string, so readiness can exercise a real lookup without
	// configuration.
	probePage string
	probeLang string
}

// Version orders snapshots across pods. Import time is assigned by whichever
// pod pulled from upstream, so a follower adopting a snapshot keeps the
// origin's version rather than stamping its own.
func (s *Snapshot) Version() int64 {
	if s == nil {
		return 0
	}
	return s.ImportedAt.UnixMilli()
}

func (s *Snapshot) Age(now time.Time) time.Duration {
	if s == nil {
		return 0
	}
	return now.Sub(s.ImportedAt)
}

// NewSnapshot builds a snapshot from parsed XLSX rows. Page and language
// strings repeat once per key, so they are interned: the map keys share one
// backing string each instead of one copy per row.
func NewSnapshot(appID, hash string, importedAt time.Time, rows []StringRow) *Snapshot {
	intern := make(map[string]string, 64)
	pool := func(s string) string {
		if v, ok := intern[s]; ok {
			return v
		}
		intern[s] = s
		return s
	}

	byPage := make(map[string]map[string]map[string]string)
	for _, r := range rows {
		page, lang := pool(r.Page), pool(r.Lang)

		byLang, ok := byPage[page]
		if !ok {
			byLang = make(map[string]map[string]string, 8)
			byPage[page] = byLang
		}
		kv, ok := byLang[lang]
		if !ok {
			kv = make(map[string]string, 32)
			byLang[lang] = kv
		}
		// Last one wins for duplicate (page, key, lang) rows in one XLSX,
		// matching the import's ON CONFLICT semantics.
		kv[r.Key] = r.Value
	}

	s := &Snapshot{
		AppID:      appID,
		Hash:       hash,
		ImportedAt: importedAt.UTC(),
		RowCount:   len(rows),
		byPage:     byPage,
	}
	s.probePage, s.probeLang = pickProbe(byPage)
	return s
}

// pickProbe chooses a deterministic non-empty (page, lang) pair. Sorting
// keeps the choice stable across pods holding identical content, so the
// readiness check is the same everywhere.
func pickProbe(byPage map[string]map[string]map[string]string) (string, string) {
	pages := make([]string, 0, len(byPage))
	for p := range byPage {
		pages = append(pages, p)
	}
	sort.Strings(pages)

	for _, p := range pages {
		langs := make([]string, 0, len(byPage[p]))
		for l := range byPage[p] {
			langs = append(langs, l)
		}
		sort.Strings(langs)

		for _, l := range langs {
			if len(byPage[p][l]) > 0 {
				return p, l
			}
		}
	}
	return "", ""
}

// Servable reports whether the snapshot holds data a request could return.
func (s *Snapshot) Servable() bool {
	if s == nil || s.RowCount == 0 || s.probePage == "" {
		return false
	}
	return len(s.byPage[s.probePage][s.probeLang]) > 0
}

// Get returns the strings for each requested page, along with the cleaned
// page list. Unknown pages yield an empty map so the response shape does not
// depend on which pages exist.
func (s *Snapshot) Get(pages []string, lang string) (map[string]map[string]string, []string) {
	cleaned := cleanPages(pages)

	out := make(map[string]map[string]string, len(cleaned))
	for _, p := range cleaned {
		if byLang, ok := s.byPage[p]; ok {
			if kv, ok := byLang[lang]; ok {
				out[p] = kv
				continue
			}
		}
		out[p] = emptyStrings
	}
	return out, cleaned
}

// cleanPages trims, drops blanks, and de-duplicates while preserving the
// order the caller asked for.
func cleanPages(pages []string) []string {
	cleaned := make([]string, 0, len(pages))
	seen := make(map[string]struct{}, len(pages))

	for _, p := range pages {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		cleaned = append(cleaned, p)
	}
	return cleaned
}

// Holder carries the current snapshot for one application. Reads are a
// single atomic load, so an import never blocks a request.
type Holder struct {
	appID string
	ptr   atomic.Pointer[Snapshot]
}

func (h *Holder) AppID() string     { return h.appID }
func (h *Holder) Load() *Snapshot   { return h.ptr.Load() }
func (h *Holder) Store(s *Snapshot) { h.ptr.Store(s) }

// StoreIfNewer adopts s only when it is strictly newer than what the holder
// already has. A pod must never move backwards to content it has already
// stopped serving, even if a lagging peer offers it.
func (h *Holder) StoreIfNewer(s *Snapshot) bool {
	for {
		cur := h.ptr.Load()
		if cur != nil {
			if s.Hash == cur.Hash || s.Version() <= cur.Version() {
				return false
			}
		}
		if h.ptr.CompareAndSwap(cur, s) {
			return true
		}
	}
}

// Registry holds one Holder per configured application. The map is built at
// startup and never mutated, so lookups need no lock.
type Registry struct {
	holders map[string]*Holder
	ids     []string
}

func NewRegistry(appIDs []string) *Registry {
	r := &Registry{holders: make(map[string]*Holder, len(appIDs))}
	for _, id := range appIDs {
		r.holders[id] = &Holder{appID: id}
		r.ids = append(r.ids, id)
	}
	sort.Strings(r.ids)
	return r
}

func (r *Registry) IDs() []string { return r.ids }

func (r *Registry) Holder(appID string) (*Holder, bool) {
	h, ok := r.holders[appID]
	return h, ok
}

// Servable reports whether at least one application has data. A single
// broken application must not make the pod unready for the healthy ones.
func (r *Registry) Servable() bool {
	for _, h := range r.holders {
		if h.Load().Servable() {
			return true
		}
	}
	return false
}
