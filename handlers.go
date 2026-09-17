package main

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rs/cors"
)

type Server struct {
	cfg      Config
	registry *Registry
	state    *State
	syncer   *Syncer
	elector  Elector
	peers    *PeerClient
	logger   *slog.Logger

	now func() time.Time
}

func NewServer(cfg Config, registry *Registry, state *State, syncer *Syncer, elector Elector, logger *slog.Logger) *Server {
	return &Server{
		cfg:      cfg,
		registry: registry,
		state:    state,
		syncer:   syncer,
		elector:  elector,
		logger:   logger,
		now:      time.Now,
	}
}

type StringsResponse struct {
	App     string                       `json:"app"`
	Lang    string                       `json:"lang"`
	Pages   []string                     `json:"pages"`
	Strings map[string]map[string]string `json:"strings"`
}

type HealthResponse struct {
	Status string `json:"status"`
}

type ReadyResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// AppStatus is one application's view from a single pod. /cluster reuses it
// verbatim, so a pod reports the same shape about itself either way.
type AppStatus struct {
	App           string `json:"app"`
	Source        string `json:"source"`
	Hash          string `json:"hash"`
	Rows          int    `json:"rows"`
	ImportedAt    string `json:"imported_at"`
	AgeSeconds    int64  `json:"age_seconds"`
	LastPull      string `json:"last_pull"`
	LastPullError string `json:"last_pull_error"`
	Failures      int    `json:"consecutive_failures"`
	Servable      bool   `json:"servable"`
}

// PodStatus is everything a pod knows about itself.
type PodStatus struct {
	Pod           string      `json:"pod"`
	Version       string      `json:"version"`
	Primary       bool        `json:"primary"`
	PrimaryHolder string      `json:"primary_holder"`
	Ready         bool        `json:"ready"`
	UptimeSeconds int64       `json:"uptime_seconds"`
	Apps          []AppStatus `json:"apps"`
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /strings", s.handleStrings)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)

	// Recovery wraps logging, not the other way round: a panicking request
	// must still emit its request log line.
	h := withRecovery(mux, s.logger)
	h = withRequestLogging(h, s.logger)

	c := cors.New(cors.Options{
		AllowedOrigins: []string{
			"*",
		},
		AllowedMethods: []string{"GET", "OPTIONS"},
		AllowedHeaders: []string{
			"Accept",
			"Authorization",
			"Content-Type",
			"If-None-Match",
			"X-Requested-With",
		},
		ExposedHeaders: []string{"ETag"},
		MaxAge:         300,
	})

	return c.Handler(h)
}

// handleHealthz is liveness. It reports the process, never its dependencies:
// a restart cannot fix an unreachable upstream, and restarting on one would
// turn a data problem into an outage. The one exception is a sync loop that
// has stopped ticking, which a restart does fix.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if stall := s.heartbeatStall(); stall > 0 {
		writeErr(w, http.StatusServiceUnavailable, "sync_stalled",
			"sync loop has not ticked for "+stall.Truncate(time.Second).String(), nil)
		return
	}
	writeOK(w, http.StatusOK, HealthResponse{Status: "ok"})
}

// heartbeatStall reports how far past the allowed window the sync loop is,
// or zero when it is ticking. The window is generous: this restarts pods.
func (s *Server) heartbeatStall() time.Duration {
	window := 3 * max(s.cfg.PullInterval, s.cfg.PeerPollInterval)
	since := s.now().Sub(s.state.LastBeat())
	if since > window {
		return since
	}
	return 0
}

// handleReadyz is readiness: can this pod serve /strings right now? It reads
// the same snapshots the handler reads, so it cannot pass while the API
// fails. Nothing about leader election, the API server, peers, or upstream
// may influence it -- otherwise a control-plane blip makes every pod unready
// at once and turns stale data into a total outage.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !s.registry.Servable() {
		writeErr(w, http.StatusServiceUnavailable, "no_data",
			"no application has servable translations yet", nil)
		return
	}
	writeOK(w, http.StatusOK, ReadyResponse{Status: "ready"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, s.podStatus())
}

func (s *Server) podStatus() PodStatus {
	now := s.now()

	apps := make([]AppStatus, 0, len(s.registry.IDs()))
	for _, id := range s.registry.IDs() {
		apps = append(apps, s.appStatus(id, now))
	}

	return PodStatus{
		Pod:           s.cfg.PodName,
		Version:       version,
		Primary:       s.elector.IsPrimary(),
		PrimaryHolder: s.elector.Primary(),
		Ready:         s.registry.Servable(),
		UptimeSeconds: int64(now.Sub(s.state.StartedAt).Seconds()),
		Apps:          apps,
	}
}

func (s *Server) appStatus(appID string, now time.Time) AppStatus {
	st := AppStatus{App: appID, Source: s.syncer.SourceName(appID)}

	lastPull, lastErr, failures := s.state.App(appID).snapshot()
	st.LastPullError = lastErr
	st.Failures = failures
	if !lastPull.IsZero() {
		st.LastPull = lastPull.UTC().Format(time.RFC3339)
	}

	h, ok := s.registry.Holder(appID)
	if !ok {
		return st
	}
	snap := h.Load()
	if snap == nil {
		return st
	}

	st.Hash = snap.Hash
	st.Rows = snap.RowCount
	st.ImportedAt = snap.ImportedAt.Format(time.RFC3339)
	st.AgeSeconds = int64(snap.Age(now).Seconds())
	st.Servable = snap.Servable()
	return st
}

func (s *Server) handleStrings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	appID := strings.TrimSpace(q.Get("app"))
	if appID == "" {
		appID = defaultAppID
	}
	holder, ok := s.registry.Holder(appID)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown_app", "unknown application: "+appID, nil)
		return
	}

	// Language codes are case-insensitive (BCP 47); import stores lowercase.
	lang := strings.ToLower(strings.TrimSpace(q.Get("lang")))
	if lang == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "missing required query param: lang", map[string]string{
			"lang": "required",
		})
		return
	}

	pages := requestedPages(q)
	if len(pages) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "missing required query param: page (repeat) or pages (csv)", map[string]string{
			"page":  "repeatable",
			"pages": "csv",
		})
		return
	}

	// One load serves both the ETag and the body, so the two can never come
	// from different imports.
	snap := holder.Load()
	if snap == nil {
		writeErr(w, http.StatusServiceUnavailable, "no_data", "no translations loaded for application: "+appID, nil)
		return
	}

	// Any given URL's content is fully determined by the last import, so the
	// import hash is a valid ETag for every /strings URL.
	etag := `"` + snap.Hash + `"`
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		setCacheHeaders(w, etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	stringsByPage, cleanedPages := snap.Get(pages, lang)

	setCacheHeaders(w, etag)
	writeOK(w, http.StatusOK, StringsResponse{
		App:     appID,
		Lang:    lang,
		Pages:   cleanedPages,
		Strings: stringsByPage,
	})
}

// requestedPages accepts repeated ?page= or a single CSV ?pages=.
func requestedPages(q map[string][]string) []string {
	pages := make([]string, 0)

	for _, p := range q["page"] {
		if p = strings.TrimSpace(p); p != "" {
			pages = append(pages, p)
		}
	}
	if len(pages) > 0 {
		return pages
	}

	for _, csv := range q["pages"] {
		for _, p := range strings.Split(csv, ",") {
			if p = strings.TrimSpace(p); p != "" {
				pages = append(pages, p)
			}
		}
	}
	return pages
}

func setCacheHeaders(w http.ResponseWriter, etag string) {
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=60")
}

func etagMatches(ifNoneMatch, etag string) bool {
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}
