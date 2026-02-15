package main

import (
	"log/slog"
	"net/http"
)

func withRecovery(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if logger != nil {
					logger.Error("panic recovered", slog.Any("panic", rec))
				}
				writeErr(w, http.StatusInternalServerError, "internal_error", "unexpected server error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
