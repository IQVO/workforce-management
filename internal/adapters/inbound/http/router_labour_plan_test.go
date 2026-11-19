package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
)

// labourPlanRouter is the test router with a labour-plan copy holding 6 heads
// at 25 units/h for WH1 / shift-1 / pack.
func labourPlanRouter(t *testing.T) http.Handler {
	t.Helper()
	copyRepo := memory.NewLabourRequirementRepo()
	if err := copyRepo.Save(context.Background(), labourplan.Requirement{
		Key:          labourplan.Key{SiteCode: "WH1", ShiftId: "shift-1", PathId: "pack"},
		PlannedHeads: 6, RateUnitsPerHour: 25, Hours: 8, HeadsSource: labourplan.HeadsSourceForecast, Version: 2,
	}); err != nil {
		t.Fatal(err)
	}
	handler := newTestHandler()
	handler.ProposePathPlan.Requirements = copyRepo
	return NewRouter(handler, testLogger, "")
}

func TestProposePathPlan_FromThePublishedLabourPlan(t *testing.T) {
	rec := doRequest(t, labourPlanRouter(t), http.MethodPost, "/paths/pack/plan-proposals", proposePathPlanRequest{SiteCode: "WH1", ShiftId: "shift-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp proposePathPlanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ProposedHeads != 6 || resp.ResolvedRate != 25 || resp.RateSource != usecases.RateSourceLabourPlan || resp.TrimReason != "" {
		t.Fatalf("resp = %+v, want 6 heads at 25 from labour-plan, untrimmed", resp)
	}
}

func TestProposePathPlan_NoPublishedLabourPlanIs404(t *testing.T) {
	rec := doRequest(t, labourPlanRouter(t), http.MethodPost, "/paths/pack/plan-proposals", proposePathPlanRequest{SiteCode: "WH1", ShiftId: "shift-9"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	var p problemDetails
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != problemErrorsURIBase+"/labour-plan-not-found" || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("problem = %+v (%s), want labour-plan-not-found", p, rec.Header().Get("Content-Type"))
	}
}

// Neither a charge nor a shiftId stays 400 missing-charge, and a charge keeps
// the deprecated sizing even when a shiftId is named (ADR 0043 decision 3).
func TestProposePathPlan_ChargePathUnchangedByShiftId(t *testing.T) {
	router := labourPlanRouter(t)
	if rec := doRequest(t, router, http.MethodPost, "/paths/pack/plan-proposals", proposePathPlanRequest{SiteCode: "WH1", ShiftId: "  "}); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank shiftId without charge: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	rec := doRequest(t, router, http.MethodPost, "/paths/pack/plan-proposals", proposePathPlanRequest{SiteCode: "WH1", ShiftId: "shift-1", Charge: chargePtr(100), PlannedRate: 30})
	var resp proposePathPlanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("charge path: %d %s", rec.Code, rec.Body.String())
	}
	if resp.ProposedHeads != 4 || resp.RateSource != usecases.RateSourceCaller {
		t.Fatalf("charge path resp = %+v, want 4 heads from the caller rate", resp)
	}
}

// ADR 0043 decision 4: the committed lines and the staffing gap show the
// published requirement next to the commitment (omitted without a row), and
// a commit below the requirement is accepted.
func TestRequiredHeadsNextToTheCommitment(t *testing.T) {
	copyRepo := memory.NewLabourRequirementRepo()
	if err := copyRepo.Save(context.Background(), labourplan.Requirement{
		Key:          labourplan.Key{SiteCode: "WH1", ShiftId: "shift-1", PathId: "pack"},
		PlannedHeads: 8, RateUnitsPerHour: 25, Hours: 8, HeadsSource: labourplan.HeadsSourceRequest, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	handler := newTestHandler()
	handler.CommitShiftPlan.Requirements = copyRepo
	handler.GetStaffingGap.Requirements = copyRepo
	router := NewRouter(handler, testLogger, "")

	rec := doRequest(t, router, http.MethodPost, "/shift-plans", commitShiftPlanRequest{
		SiteCode: "WH1", ShiftId: "shift-1",
		Lines: []pathPlanLineRequest{{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40, InstalledStations: intp(10)}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("a commit below the requirement must be accepted: %d %s", rec.Code, rec.Body.String())
	}
	var plan shiftPlanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Lines) != 1 || plan.Lines[0].RequiredHeads == nil || *plan.Lines[0].RequiredHeads != 8 || plan.Lines[0].PlannedHeads != 5 {
		t.Fatalf("committed lines = %+v, want 8 required next to 5 planned", plan.Lines)
	}

	rec = doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?siteCode=WH1&shiftId=shift-1", nil)
	var gap staffingGapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &gap); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("staffing gap: %d %s", rec.Code, rec.Body.String())
	}
	if gap.RequiredHeads == nil || *gap.RequiredHeads != 8 {
		t.Fatalf("gap = %+v, want requiredHeads 8", gap)
	}

	handler.GetStaffingGap.Requirements = memory.NewLabourRequirementRepo()
	rec = doRequest(t, NewRouter(handler, testLogger, ""), http.MethodGet, "/paths/pack/staffing-gap?siteCode=WH1&shiftId=shift-1", nil)
	if bytes.Contains(rec.Body.Bytes(), []byte("requiredHeads")) {
		t.Fatalf("requiredHeads must be omitted without a published row: %s", rec.Body.String())
	}
}
