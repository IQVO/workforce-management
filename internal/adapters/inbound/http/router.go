package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riandyrn/otelchi"
	otelchimetric "github.com/riandyrn/otelchi/metric"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// DefaultServiceName is the OTel service name this adapter reports when the
// caller does not override it (via OTEL_SERVICE_NAME).
const DefaultServiceName = "workforce-management"

// Handler wires the Workforce Management use cases to chi routes.
type Handler struct {
	StartAssociateShift *usecases.StartAssociateShift
	CertifyAssociate    *usecases.CertifyAssociate
	ProposePathPlan     *usecases.ProposePathPlan
	CommitShiftPlan     *usecases.CommitShiftPlan
	AssignLabor         *usecases.AssignLabor
	StartBreak          *usecases.StartBreak
	EndBreak            *usecases.EndBreak
	GetStaffingGap      *usecases.GetStaffingGap
	EndAssociateShift   *usecases.EndAssociateShift

	// Catalogue validates every caller-supplied path_id against the
	// fleet's declared process-path catalogue before it is used to
	// propose/commit a plan or assign labor — see
	// fulfillment-execution's ADR-0017 and this service's own ADR-0013.
	// A nil Catalogue (only ever the case in older tests not yet
	// updated) skips validation rather than panicking, so this is
	// additive, not a required wiring change for every caller.
	// ports.PathCatalogue (not the concrete *pathcatalog.Catalogue) so
	// an alternative source (e.g. a Kafka-fed catalogue) can be wired
	// in without touching this struct.
	Catalogue ports.PathCatalogue

	// Readiness backs GET /readyz (ADR-0022 §graceful shutdown,
	// ported from order-management's ADR-0025): flipped to not-ready
	// as the FIRST step of the composition root's shutdown sequence,
	// before anything else stops. A nil Readiness (the Handler zero
	// value — every existing test) always reports ready.
	Readiness *Readiness

	// IdempotencyPool, when non-nil, wires RequireIdempotencyKey onto the
	// resource-creation POSTs (/shift-plans and
	// /associates/{id}/assignments, ADR-0027). It must be a real
	// *pgxpool.Pool because the middleware begins its own transaction that
	// the use case's UnitOfWork then joins. A nil pool means "no
	// transactional Postgres backing wired" (in-memory dev/test), and the
	// middleware is simply not applied.
	IdempotencyPool *pgxpool.Pool
}

// validatePathId checks pathId against h.Catalogue when one is wired
// in. Call this from every handler that accepts a caller-supplied
// path_id for a WRITE (propose/commit a plan, assign labor) — the
// read-only staffing-gap lookup also validates, since an unrecognized
// path_id there is a genuinely different failure than "no plan exists
// yet for this valid path."
func (h *Handler) validatePathId(pathId shared.PathId) error {
	if h.Catalogue == nil {
		return nil
	}
	_, err := h.Catalogue.Lookup(string(pathId))
	return err
}

// RouterOption customises NewRouter / NewReportsRouter. No options are
// defined today; the type is kept so existing call sites (and any future
// cross-cutting router configuration) do not need to change shape.
type RouterOption func(*routerOptions)

type routerOptions struct{}

// NewRouter builds the chi router for the Workforce Management REST API.
// A nil logger defaults to slog.Default(); an empty serviceName defaults to
// DefaultServiceName.
//
// Middleware order matters: otelchi runs first so a span exists (and the
// request context carries it) before RequestLogger emits its log line —
// that is what puts trace_id/span_id on request logs.
func NewRouter(h *Handler, logger *slog.Logger, serviceName string, opts ...RouterOption) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	if serviceName == "" {
		serviceName = DefaultServiceName
	}
	var o routerOptions
	for _, opt := range opts {
		opt(&o)
	}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// WithChiRoutes makes span names the route pattern
	// (POST /associates/{id}/assignments) rather than the raw path, keeping
	// span-name cardinality bounded by the route table.
	r.Use(otelchi.Middleware(serviceName, otelchi.WithChiRoutes(r)))
	r.Use(otelchimetric.NewServerRequestDuration(
		otelchimetric.NewBaseConfig(serviceName),
	))
	r.Use(RequestLogger(logger))
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware())

	r.Get("/healthz", h.healthz)
	r.Get("/readyz", h.handleReadyz)

	r.Post("/associates/{id}/start-shift", h.startShift)
	r.Post("/associates/{id}/certifications", h.certify)
	r.Post("/paths/{pathId}/plan/propose", h.proposePathPlan)
	// POST /shift-plans and POST /associates/{id}/assignments are the two
	// mutating endpoints whose blind retry after a lost response would
	// re-apply real state and re-publish events (ADR-0027). They are
	// route-scoped (r.With, not r.Use) behind RequireIdempotencyKey when an
	// IdempotencyPool is wired; a nil pool (in-memory dev/test) leaves them
	// unprotected, matching every other optional Postgres-backed capability.
	// start-shift is idempotent by associate id, certify/break/end-shift act
	// on a caller-supplied id, and propose is a pure computation.
	idempotent := r.With()
	if h.IdempotencyPool != nil {
		idempotent = r.With(RequireIdempotencyKey(h.IdempotencyPool))
	}
	idempotent.Post("/shift-plans", h.commitShiftPlan)
	idempotent.Post("/associates/{id}/assignments", h.assignLabor)
	r.Post("/associates/{id}/break/start", h.startBreak)
	r.Post("/associates/{id}/break/end", h.endBreak)
	r.Get("/paths/{pathId}/staffing-gap", h.staffingGap)
	// Canonical route (ADR 0035): the path segment is the Site code, both the
	// plan lookup key and the associate scope.
	r.Get("/sites/{siteCode}/shifts/{shiftId}/staffing-gap", h.staffingGapForSiteShift)
	// Deprecated alias of the route above (same handler core and response
	// shape): buildingId is the legacy name of the plan key. Kept working,
	// announced with `Deprecation: true`, removed only by a later ADR.
	r.With(deprecatedRoute).Get("/buildings/{buildingId}/shifts/{shiftId}/staffing-gap", h.staffingGapForShift)
	r.Post("/associates/{id}/end-shift", h.endShift)

	return r
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// corsMiddleware allows the warehouse-console browser SPA (and this
// service's own future MFE remote dev origin) to call this API directly
// from the browser. No credentials (cookies) are ever needed here.
// CORS_ALLOWED_ORIGINS overrides the local-dev default (comma-separated)
// for staging/prod deployments.
func corsMiddleware() func(http.Handler) http.Handler {
	origins := []string{"http://localhost:5173", "http://localhost:5185"}
	if v := os.Getenv("CORS_ALLOWED_ORIGINS"); v != "" {
		origins = strings.Split(v, ",")
	}
	return cors.Handler(cors.Options{
		AllowedOrigins:   origins,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete},
		AllowedHeaders:   []string{"Content-Type", "Authorization", IdempotencyKeyHeader},
		AllowCredentials: false,
		MaxAge:           300,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an RFC 7807 (application/problem+json) error response.
// category is looked up from err (falling back to a status-keyed generic
// category); detail carries the existing dynamic err.Error() text; instance
// is the request path that produced the error.
func writeError(w http.ResponseWriter, r *http.Request, status int, err error) {
	cat := categoryFor(status, err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problemDetails{
		Type:     problemErrorsURIBase + "/" + cat.slug,
		Title:    cat.title,
		Status:   status,
		Detail:   err.Error(),
		Instance: r.URL.Path,
	})
}
