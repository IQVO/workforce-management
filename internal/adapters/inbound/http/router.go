package http

import (
	"encoding/json"
	"fmt"
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
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
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
	r.Get("/buildings/{buildingId}/shifts/{shiftId}/staffing-gap", h.staffingGapForShift)
	r.Post("/associates/{id}/end-shift", h.endShift)

	return r
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) startShift(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	// Decode into a POINTER so a literal JSON null body (a no-op for a
	// value struct — every field here is optional, so null silently
	// created a shift) is distinguishable and rejected. The spec's
	// requestBody is a required object; null is malformed-request-body.
	var req *startShiftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if req == nil {
		writeError(w, r, http.StatusBadRequest, errNullBody)
		return
	}

	certs := make([]shared.Certification, len(req.Certifications))
	for i, c := range req.Certifications {
		cert, err := shared.NewCertification(c)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, err)
			return
		}
		certs[i] = cert
	}

	shift, err := h.StartAssociateShift.Execute(r.Context(), associateId, certs)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.Header().Set("Location", "/associates/"+string(shift.AssociateId()))
	writeJSON(w, http.StatusCreated, map[string]any{
		"associateId": string(shift.AssociateId()),
	})
}

func (h *Handler) certify(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	var req certifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	certification, err := shared.NewCertification(req.Certification)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	if err := h.CertifyAssociate.Execute(r.Context(), associateId, certification); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) proposePathPlan(w http.ResponseWriter, r *http.Request) {
	pathId, err := shared.NewPathId(chi.URLParam(r, "pathId"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.validatePathId(pathId); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	var req proposePathPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if req.BuildingId == "" {
		writeError(w, r, http.StatusBadRequest, errMissingBuildingId)
		return
	}
	// An omitted charge is a client mistake (explicit 0 is legitimate) —
	// without this nil-check the omitted field was silently coerced to a
	// 0 charge and the proposal accepted.
	if req.Charge == nil {
		writeError(w, r, http.StatusBadRequest, errMissingCharge)
		return
	}

	heads, resolvedRate, rateSource, trimReason, err := h.ProposePathPlan.Execute(r.Context(), req.BuildingId, pathId, *req.Charge, req.PlannedRate)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, proposePathPlanResponse{
		PathId:        string(pathId),
		ProposedHeads: heads,
		ResolvedRate:  resolvedRate,
		RateSource:    rateSource,
		TrimReason:    trimReason,
	})
}

func (h *Handler) commitShiftPlan(w http.ResponseWriter, r *http.Request) {
	var req commitShiftPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if req.BuildingId == "" {
		writeError(w, r, http.StatusBadRequest, errMissingBuildingId)
		return
	}
	if req.ShiftId == "" {
		writeError(w, r, http.StatusBadRequest, errMissingShiftId)
		return
	}

	lines := make([]shiftplan.PathPlan, len(req.Lines))
	installed := make(map[shared.PathId]int, len(req.Lines))
	for i, l := range req.Lines {
		pathId, err := shared.NewPathId(l.PathId)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, err)
			return
		}
		if err := h.validatePathId(pathId); err != nil {
			writeError(w, r, http.StatusBadRequest, err)
			return
		}
		lines[i] = shiftplan.PathPlan{
			PathId:       pathId,
			PlannedHeads: l.PlannedHeads,
			PlannedRate:  l.PlannedRate,
			PlannedHours: l.PlannedHours,
		}
		installed[pathId] = l.InstalledStations
	}

	sp, err := h.CommitShiftPlan.Execute(r.Context(), req.BuildingId, req.ShiftId, lines, installed)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}

	respLines := make([]pathPlanLineResp, 0, len(sp.Lines()))
	for _, l := range sp.Lines() {
		respLines = append(respLines, pathPlanLineResp{
			PathId:       string(l.PathId),
			PlannedHeads: l.PlannedHeads,
			PlannedRate:  l.PlannedRate,
			PlannedHours: l.PlannedHours,
		})
	}
	w.Header().Set("Location", fmt.Sprintf("/shift-plans/%s/%s", sp.BuildingId(), sp.ShiftId()))
	writeJSON(w, http.StatusCreated, shiftPlanResponse{
		BuildingId: sp.BuildingId(),
		ShiftId:    sp.ShiftId(),
		Lines:      respLines,
	})
}

func (h *Handler) assignLabor(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	var req assignLaborRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	pathId, err := shared.NewPathId(req.PathId)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.validatePathId(pathId); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}

	la, err := h.AssignLabor.Execute(r.Context(), associateId, pathId)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}

	activePathId, active := la.ActivePathId()
	w.Header().Set("Location", "/associates/"+string(associateId)+"/assignments")
	writeJSON(w, http.StatusCreated, assignmentResponse{
		AssociateId:  string(la.AssociateId()),
		ActivePathId: string(activePathId),
		Active:       active,
	})
}

func (h *Handler) startBreak(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.StartBreak.Execute(r.Context(), associateId); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) endBreak(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.EndBreak.Execute(r.Context(), associateId); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// staffingGap answers GET /paths/{pathId}/staffing-gap?buildingId=&shiftId=.
// ShiftPlan is keyed by building+shift, so this context requires those as
// query parameters alongside the path in the URL.
func (h *Handler) staffingGap(w http.ResponseWriter, r *http.Request) {
	pathId, err := shared.NewPathId(chi.URLParam(r, "pathId"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.validatePathId(pathId); err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	buildingId := r.URL.Query().Get("buildingId")
	if buildingId == "" {
		writeError(w, r, http.StatusBadRequest, errMissingBuildingId)
		return
	}
	shiftId := r.URL.Query().Get("shiftId")
	if shiftId == "" {
		writeError(w, r, http.StatusBadRequest, errMissingShiftId)
		return
	}

	gap, err := h.GetStaffingGap.Execute(r.Context(), buildingId, shiftId, pathId)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, toStaffingGapResponse(gap))
}

// staffingGapForShift answers GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap
// -- the fleet-wide "all paths, one building/shift" list ADR-0011 flagged
// as a deferred fast-follow ("the shipped screen exposes a real gap in
// this context's own read-model surface (no list-by-building/shift
// endpoint)"). It answers the staffing gap for EVERY path planned in the
// committed shift plan, reusing the exact same per-path computation as
// the single-path lookup above (GetStaffingGap.ExecuteAll).
//
// buildingId/shiftId are chi PATH parameters here (unlike the single-path
// lookup's query parameters above): chi never matches a {param} route
// segment against an empty string, so there is no empty-value case to
// validate for either -- a malformed request simply 404s before reaching
// this handler at all.
func (h *Handler) staffingGapForShift(w http.ResponseWriter, r *http.Request) {
	buildingId := chi.URLParam(r, "buildingId")
	shiftId := chi.URLParam(r, "shiftId")

	gaps, err := h.GetStaffingGap.ExecuteAll(r.Context(), buildingId, shiftId)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, toStaffingGapResponses(gaps))
}

func (h *Handler) endShift(w http.ResponseWriter, r *http.Request) {
	associateId, err := shared.NewAssociateId(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := h.EndAssociateShift.Execute(r.Context(), associateId); err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
