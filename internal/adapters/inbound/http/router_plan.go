package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
	// No charge: the proposal comes from warehouse-planning's published
	// labour plan for the named shift (ADR 0043). Neither a charge nor a
	// shiftId is a client mistake (explicit 0 charge is legitimate) —
	// without this nil-check the omitted field was silently coerced to a
	// 0 charge and the proposal accepted.
	if req.Charge == nil {
		shiftId := strings.TrimSpace(req.ShiftId)
		if shiftId == "" {
			writeError(w, r, http.StatusBadRequest, errMissingCharge)
			return
		}
		h.proposeFromLabourPlan(w, r, siteKey, shiftId, pathId)
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

// proposeFromLabourPlan answers a proposal without a charge from the local
// copy of the labour plan: heads as published, no trim, 404
// labour-plan-not-found when nothing was published (ADR 0043 decision 2).
func (h *Handler) proposeFromLabourPlan(w http.ResponseWriter, r *http.Request, siteKey, shiftId string, pathId shared.PathId) {
	heads, resolvedRate, rateSource, err := h.ProposePathPlan.ExecuteFromLabourPlan(r.Context(), siteKey, shiftId, pathId)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, proposePathPlanResponse{
		PathId:        string(pathId),
		ProposedHeads: heads,
		ResolvedRate:  resolvedRate,
		RateSource:    rateSource,
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
		// installedStations is REQUIRED (openapi PathPlanLineRequest): an
		// omitted or null count is refused here, before the live capacity
		// lookups, rather than decoded as 0 and answered as a 409
		// planned-heads-exceed-installed. An explicit 0 is a stated count.
		if l.InstalledStations == nil {
			writeError(w, r, statusFor(shiftplan.ErrMissingInstalledStations), shiftplan.ErrMissingInstalledStations)
			return
		}
		lines[i] = shiftplan.PathPlan{
			PathId:       pathId,
			PlannedHeads: l.PlannedHeads,
			PlannedRate:  l.PlannedRate,
			PlannedHours: l.PlannedHours,
		}
		installed[pathId] = *l.InstalledStations
	}

	sp, err := h.CommitShiftPlan.Execute(r.Context(), siteKey, req.ShiftId, lines, installed)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}

	// The plan is committed: the requirement is only shown next to each
	// line (ADR 0043 decision 4), never compared with it.
	required, err := h.CommitShiftPlan.RequiredHeads(r.Context(), sp)
	if err != nil {
		writeError(w, r, statusFor(err), err)
		return
	}
	respLines := make([]pathPlanLineResp, 0, len(sp.Lines()))
	for _, l := range sp.Lines() {
		line := pathPlanLineResp{
			PathId:       string(l.PathId),
			PlannedHeads: l.PlannedHeads,
			PlannedRate:  l.PlannedRate,
			PlannedHours: l.PlannedHours,
		}
		if heads, ok := required[l.PathId]; ok {
			line.RequiredHeads = &heads
		}
		respLines = append(respLines, line)
	}
	w.Header().Set("Location", fmt.Sprintf("/shift-plans/%s/%s", sp.SiteCode(), sp.ShiftId()))
	writeJSON(w, http.StatusCreated, shiftPlanResponse{
		SiteCode:   sp.SiteCode(),
		BuildingId: sp.BuildingId(),
		ShiftId:    sp.ShiftId(),
		Lines:      respLines,
	})
}
