package http

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// statusKey labels the HTTP status code in structured request logs and
// readiness/health JSON bodies.
const statusKey = "status"

// sanitizeForLog strips CR/LF from an attacker-controlled value (here,
// the raw request path) before it is written to a log line. Without this,
// a crafted path segment containing an encoded newline could forge a fake
// log entry that appears to be a separate, legitimate line (CWE-117 log
// injection) once the log record reaches a downstream viewer/aggregator
// that doesn't preserve slog's JSON string escaping.
func sanitizeForLog(s string) string {
	return strings.NewReplacer("\n", "", "\r", "").Replace(s)
}

// RequestLogger returns chi middleware that logs each request's method,
// route, status, duration, and response size via logger. Requests that
// result in a 5xx status are logged at Error; everything else at Info.
//
// It logs with the request context (InfoContext/ErrorContext), so when the
// configured handler is telemetry.TraceHandler and a tracing middleware has
// already started a span, each line carries trace_id/span_id.
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(ww, r)
			duration := time.Since(start)

			ctx := r.Context()
			attrs := []any{
				"method", r.Method,
				"path", sanitizeForLog(r.URL.Path),
				"route", routePattern(ctx),
				statusKey, ww.Status(),
				"duration_ms", duration.Milliseconds(),
				"bytes", ww.BytesWritten(),
				"request_id", middleware.GetReqID(ctx),
			}
			if ww.Status() >= http.StatusInternalServerError {
				logger.ErrorContext(ctx, "http request", attrs...)
			} else {
				logger.InfoContext(ctx, "http request", attrs...)
			}
		})
	}
}

// routePattern returns the matched chi route pattern (e.g.
// /associates/{id}/assignments), or "" for an unmatched request. Logging the
// pattern alongside the raw path gives log aggregation the same
// low-cardinality grouping key the trace spans use.
func routePattern(ctx context.Context) string {
	if rctx := chi.RouteContext(ctx); rctx != nil {
		return rctx.RoutePattern()
	}
	return ""
}
