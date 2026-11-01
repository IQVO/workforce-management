package http

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

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
	siteKey, err := shared.ResolveSiteKey(req.SiteCode, req.BuildingId)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	// An omitted charge is a client mistake (explicit 0 is legitimate) —
	// without this nil-check the omitted field was silently coerced to a
	// 0 charge and the proposal accepted.
	if req.Charge == nil {
		writeError(w, r, http.StatusBadRequest, errMissingCharge)
		return
	}

	heads, resolvedRate, rateSource, trimReason, err := h.ProposePathPlan.Execute(r.Context(), siteKey, pathId, *req.Charge, req.PlannedRate)
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
	siteKey, err := shared.ResolveSiteKey(req.SiteCode, req.BuildingId)
	if err != nil {
		writeError(w, r, statusFor(err), err)
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

	sp, err := h.CommitShiftPlan.Execute(r.Context(), siteKey, req.ShiftId, lines, installed)
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
	w.Header().Set("Location", fmt.Sprintf("/shift-plans/%s/%s", sp.SiteCode(), sp.ShiftId()))
	writeJSON(w, http.StatusCreated, shiftPlanResponse{
		SiteCode:   sp.SiteCode(),
		BuildingId: sp.BuildingId(),
		ShiftId:    sp.ShiftId(),
		Lines:      respLines,
	})
}
