package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// internalRoutes serves the pod-to-pod endpoints. They run on a separate
// listener that is absent from the Service and the ingress, because the
// public ingress routes "/" as a prefix and would otherwise expose them.
func (s *Server) internalRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+internalPeerPath, s.handleInternalPeer)
	mux.HandleFunc("GET "+internalSnapshotPath, s.handleInternalSnapshot)

	// Logging wraps recovery, matching the public listener.
	h := withRecovery(mux, s.logger)
	return withRequestLogging(h, s.logger)
}

// handleInternalPeer answers with this pod's own view only. It never calls
// out to other peers: every pod polls every other pod, and a fan-out here
// would turn that into a storm.
func (s *Server) handleInternalPeer(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.peerInfo())
}

func (s *Server) peerInfo() PeerInfo {
	apps := make([]PeerAppInfo, 0, len(s.registry.IDs()))
	for _, id := range s.registry.IDs() {
		info := PeerAppInfo{App: id}
		lastPull, lastErr, failures := s.state.App(id).snapshot()
		info.LastPullError, info.Failures = lastErr, failures
		if !lastPull.IsZero() {
			info.LastPullAt = lastPull.UTC().Format(time.RFC3339Nano)
		}

		if h, ok := s.registry.Holder(id); ok {
			if snap := h.Load(); snap != nil {
				info.Hash = snap.Hash
				info.ImportedAt = snap.ImportedAt.Format(time.RFC3339Nano)
				info.Rows = snap.RowCount
				info.Servable = snap.Servable()
			}
		}
		apps = append(apps, info)
	}

	return PeerInfo{
		Pod:     s.cfg.PodName,
		Version: version,
		Primary: s.elector.IsPrimary(),
		Ready:   s.registry.Servable(),
		Apps:    apps,
	}
}

// handleInternalSnapshot streams the XLSX a snapshot was built from. The
// hash travels in a header so the receiver can verify the bytes rather than
// trust the sender.
func (s *Server) handleInternalSnapshot(w http.ResponseWriter, r *http.Request) {
	appID := strings.TrimSpace(r.URL.Query().Get("app"))
	if appID == "" {
		appID = defaultAppID
	}

	holder, ok := s.registry.Holder(appID)
	if !ok {
		http.Error(w, "unknown application: "+appID, http.StatusNotFound)
		return
	}
	snap := holder.Load()
	if snap == nil || len(snap.Raw()) == 0 {
		http.Error(w, "no snapshot for application: "+appID, http.StatusServiceUnavailable)
		return
	}

	w.Header().Set(headerSnapshotHash, snap.Hash)
	w.Header().Set(headerSnapshotImportedAt, snap.ImportedAt.Format(time.RFC3339Nano))
	w.Header().Set(headerSnapshotRows, strconv.Itoa(snap.RowCount))
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Length", strconv.Itoa(len(snap.Raw())))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(snap.Raw())
}
