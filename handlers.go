package main

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/rs/cors"
)

type Server struct {
	db     *DB
	ready  *ReadyState
	logger *slog.Logger
}

type StringsResponse struct {
	Lang    string                       `json:"lang"`
	Pages   []string                     `json:"pages"`
	Strings map[string]map[string]string `json:"strings"`
}

type HealthResponse struct {
	Status string `json:"status"`
}

type ReadyResponse struct {
	Status string `json:"status"`
}

type StatusResponse struct {
	LastPull string `json:"last_pull"`
	LastHash string `json:"last_hash"`
	Ready    bool   `json:"ready"`
	Version  string `json:"version"`
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /strings", s.handleGetStrings)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)

	h := withRequestLogging(mux, s.logger)
	h = withRecovery(h, s.logger)

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

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, HealthResponse{Status: "ok"})
}

// handleReadyz always reports ready while the process is up: the alpha
// deploy tooling restarts pods that stay unready past a limit, and with a
// single replica gating traffic gains nothing (there is no healthy
// alternative pod). The real "can serve" signal is `ready` on /status.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, ReadyResponse{Status: "ready"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	lastPull, _, err := s.db.GetMeta(r.Context(), metaKeyLastPull)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to read status", nil)
		return
	}
	lastHash, _, err := s.db.GetMeta(r.Context(), metaKeyLastHash)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to read status", nil)
		return
	}

	writeOK(w, http.StatusOK, StatusResponse{
		LastPull: lastPull,
		LastHash: lastHash,
		Ready:    s.ready.IsReady(),
		Version:  version,
	})
}

func (s *Server) handleGetStrings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Language codes are case-insensitive (BCP 47); import stores lowercase.
	lang := strings.ToLower(strings.TrimSpace(q.Get("lang")))
	if lang == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "missing required query param: lang", map[string]string{
			"lang": "required",
		})
		return
	}

	pages := make([]string, 0)

	if pageValues, ok := q["page"]; ok {
		for _, p := range pageValues {
			p = strings.TrimSpace(p)
			if p != "" {
				pages = append(pages, p)
			}
		}
	}

	if len(pages) == 0 {
		pagesCSV := strings.TrimSpace(q.Get("pages"))
		if pagesCSV != "" {
			for _, p := range strings.Split(pagesCSV, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					pages = append(pages, p)
				}
			}
		}
	}

	if len(pages) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "missing required query param: page (repeat) or pages (csv)", map[string]string{
			"page":  "repeatable",
			"pages": "csv",
		})
		return
	}

	// Any given URL's content is fully determined by the last import,
	// so the import hash is a valid ETag for every /strings URL.
	// Only 200/304 responses carry the caching headers.
	var etag string
	if hash, ok, err := s.db.GetMeta(r.Context(), metaKeyLastHash); err == nil && ok {
		etag = `"` + hash + `"`
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			setCacheHeaders(w, etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	stringsByPage, cleanedPages, err := s.db.GetStringsByPagesLang(r.Context(), pages, lang)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to fetch strings", nil)
		return
	}

	if etag != "" {
		setCacheHeaders(w, etag)
	}
	writeOK(w, http.StatusOK, StringsResponse{
		Lang:    lang,
		Pages:   cleanedPages,
		Strings: stringsByPage,
	})
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
