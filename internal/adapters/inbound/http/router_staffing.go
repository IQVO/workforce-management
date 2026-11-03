package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// staffingGap answers GET /paths/{pathId}/staffing-gap?siteCode=&shiftId=[&buildingId=].
// ShiftPlan is keyed by site+shift, so this context requires those as query
// parameters alongside the path in the URL. siteCode is the canonical name:
// plan lookup key AND associate scope (ADR 0034/0035). buildingId is the
// deprecated alias of the plan lookup ONLY: a call that gives just buildingId
// is not scoped (fleet-wide count, behaviour identical to before). One of the
// two is required.
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
	planKey, scope, err := shared.ResolveGapLookup(r.URL.Query().Get("siteCode"), r.URL.Query().Get("buildingId"))
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	shiftId := r.URL.Query().Get("shiftId")
	if shiftId == "" {
		writeError(w, r, http.StatusBadRequest, errMissingShiftId)
		return
	}

	gap, err := h.GetStaffingGap.ExecuteForSite(r.Context(), planKey, shiftId, pathId, scope)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, toStaffingGapResponse(gap))
}

// deprecatedRoute marks a route's every response `Deprecation: true` (the
// IETF Deprecation header, RFC 9745) -- including its error responses -- so a
// client of the legacy buildings route learns about the canonical sites route.
func deprecatedRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Deprecation", "true")
		next.ServeHTTP(w, r)
	})
}

// staffingGapForSiteShift answers the canonical
// GET /sites/{siteCode}/shifts/{shiftId}/staffing-gap (ADR 0035): the same
// fleet-wide "all paths, one site/shift" list as staffingGapForShift, with
// the path siteCode as both the plan lookup key and the associate scope.
// There is no second name on this route, so nothing can conflict; the path
// siteCode is the resource identity, used exactly as given (ParsePathSiteKey).
func (h *Handler) staffingGapForSiteShift(w http.ResponseWriter, r *http.Request) {
	siteCode, err := shared.ParsePathSiteKey(chi.URLParam(r, "siteCode"))
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	shiftId := chi.URLParam(r, "shiftId")
	limit, after, ok := pageParams(w, r)
	if !ok {
		return
	}

	gaps, err := h.GetStaffingGap.ExecuteAllForSite(r.Context(), siteCode, shiftId, shared.NewSiteCode(siteCode))
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, staffingGapPage(gaps, limit, after))
}

// staffingGapForShift answers the DEPRECATED
// GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap (the canonical
// route is /sites/{siteCode}/shifts/{shiftId}/staffing-gap, ADR 0035) --
// the fleet-wide "all paths, one building/shift" list ADR-0011 flagged
// as a deferred fast-follow ("the shipped screen exposes a real gap in
// this context's own read-model surface (no list-by-building/shift
// endpoint)"). It answers the staffing gap for EVERY path planned in the
// committed shift plan, reusing the exact same per-path computation as
// the single-path lookup above (GetStaffingGap.ExecuteAll). buildingId is the
// plan key only: the list is unscoped unless the optional siteCode query
// parameter is given (ADR 0034).
//
// buildingId/shiftId are chi PATH parameters here (unlike the single-path
// lookup's query parameters above): chi never matches a {param} route
// segment against an empty string, so there is no empty-value case to
// validate for either -- a malformed request simply 404s before reaching
// this handler at all.
func (h *Handler) staffingGapForShift(w http.ResponseWriter, r *http.Request) {
	buildingId := chi.URLParam(r, "buildingId")
	shiftId := chi.URLParam(r, "shiftId")
	limit, after, ok := pageParams(w, r, "siteCode")
	if !ok {
		return
	}

	gaps, err := h.GetStaffingGap.ExecuteAllForSite(r.Context(), buildingId, shiftId, shared.NewSiteCode(r.URL.Query().Get("siteCode")))
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, staffingGapPage(gaps, limit, after))
}
