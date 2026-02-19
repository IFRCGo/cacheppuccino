package main

import (
	"log/slog"
	"net/http"
	"strings"
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
	return h
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, HealthResponse{Status: "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// FIXME: we should return actual ready state
	// if s.ready != nil && s.ready.IsReady() {
	// 	writeOK(w, http.StatusOK, ReadyResponse{Status: "ready"})
	// 	return
	// }
	// writeErr(w, http.StatusServiceUnavailable, "not_ready", "not ready", nil)

	// FIXME: we're temporarily always ready
	writeOK(w, http.StatusOK, ReadyResponse{Status: "ready"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	lastPull, _, _ := s.db.GetMeta(r.Context(), "last_pull_rfc3339")
	lastHash, _, _ := s.db.GetMeta(r.Context(), "last_xlsx_sha256")

	writeOK(w, http.StatusOK, StatusResponse{
		LastPull: lastPull,
		LastHash: lastHash,
		Ready:    s.ready != nil && s.ready.IsReady(),
	})
}

func (s *Server) handleGetStrings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	lang := strings.TrimSpace(q.Get("lang"))
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

	stringsByPage, cleanedPages, err := s.db.GetStringsByPagesLang(r.Context(), pages, lang)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to fetch strings", nil)
		return
	}

	writeOK(w, http.StatusOK, StringsResponse{
		Lang:    lang,
		Pages:   cleanedPages,
		Strings: stringsByPage,
	})
}
