package main

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

func withRecovery(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if logger != nil {
				// Without the stack a panic report names the value and
				// nothing about where it came from.
				logger.Error("panic recovered",
					slog.Any("panic", rec),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
			}
			writeErr(w, http.StatusInternalServerError, "internal_error", "unexpected server error", nil)
		}()
		next.ServeHTTP(w, r)
	})
}
