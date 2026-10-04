package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/riandyrn/otelchi"
	otelchimetric "github.com/riandyrn/otelchi/metric"
)

// newRouter wraps the MCP handler in a small chi router so the binary is
// deployable behind Kubernetes probes and carries the same HTTP RED
// instrumentation as every other fleet service (ADR-0015 Tier 1):
//
//   - otelchi.Middleware starts the request span (named after the route
//     pattern, via WithChiRoutes) and otelchimetric records
//     http.server.request.duration, in that order.
//   - GET /healthz -> 200 {"status":"ok"}. Liveness and readiness probes hit
//     this route. MCP is unauthenticated by decision, so probes need no
//     credentials.
//   - /  and /mcp   -> the MCP Streamable HTTP handler. Both paths are served
//     so warehouse-ops-agent's *_MCP_ENDPOINT convention (".../mcp") and the
//     pre-existing root-mounted endpoint keep working without a client-side
//     change.
//
// serviceName is the OTEL_SERVICE_NAME resolved by the composition root.
func newRouter(mcpHandler http.Handler, serviceName string) http.Handler {
	r := chi.NewRouter()
	r.Use(otelchi.Middleware(serviceName, otelchi.WithChiRoutes(r)))
	r.Use(otelchimetric.NewServerRequestDuration(otelchimetric.NewBaseConfig(serviceName)))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	r.Mount("/mcp", mcpHandler)
	r.Mount("/", mcpHandler)
	return r
}
