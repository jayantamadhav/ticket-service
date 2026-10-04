// Package logging provides structured, request-correlated logging shared
// across main and the handlers package.
package logging

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

type ctxKey string

const loggerKey ctxKey = "logger"

// WithContext attaches a logger (already annotated with a request id) to the
// request context so handlers and the store layer can pull it back out.
func WithContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// FromContext returns the request-scoped logger, falling back to a bare
// default logger if none was attached (e.g. in tests).
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// RequestLogger is chi-compatible middleware that logs one structured line
// per request (method, path, status, duration, request id) and attaches a
// request-scoped logger to the context for handlers to enrich further.
func RequestLogger(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reqID := middleware.GetReqID(r.Context())

			reqLogger := base.With("request_id", reqID)
			ctx := WithContext(r.Context(), reqLogger)

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r.WithContext(ctx))

			reqLogger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"duration_ms", time.Since(start).Milliseconds(),
				"remote_addr", r.RemoteAddr,
			)
		})
	}
}
