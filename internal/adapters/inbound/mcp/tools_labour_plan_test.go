package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
)

// chargeOf builds the optional (deprecated) charge of proposeHeadsInput.
func chargeOf(v float64) *float64 { return &v }

// ADR 0043: propose_path_heads without a charge reads the published labour
// plan for (siteCode, shiftId, pathId): heads as published, no trim, an error
// (never zero heads) when nothing was published.
func TestProposePathHeads_FromThePublishedLabourPlan(t *testing.T) {
	h := newHarness(t)
	copyRepo := memory.NewLabourRequirementRepo()
	if err := copyRepo.Save(context.Background(), labourplan.Requirement{
		Key:          labourplan.Key{SiteCode: "WH1", ShiftId: "shift-1", PathId: "pack"},
		PlannedHeads: 7, RateUnitsPerHour: 25, Hours: 8, HeadsSource: labourplan.HeadsSourceForecast, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	h.deps.ProposePathPlan.Requirements = copyRepo
	ctx := context.Background()

	out, err := h.deps.proposePathHeads(ctx, proposeHeadsInput{SiteCode: "WH1", ShiftId: "shift-1", PathId: "pack"})
	if err != nil {
		t.Fatal(err)
	}
	if out.ProposedHeads != 7 || out.PlannedRate != 25 || out.RateSource != usecases.RateSourceLabourPlan || out.Charge != nil || out.ShiftId != "shift-1" {
		t.Fatalf("out = %+v, want 7 heads at 25 from labour-plan, no charge", out)
	}

	if _, err := h.deps.proposePathHeads(ctx, proposeHeadsInput{SiteCode: "WH1", ShiftId: "shift-9", PathId: "pack"}); !errors.Is(err, labourplan.ErrRequirementNotFound) {
		t.Fatalf("unpublished shift: err = %v, want ErrRequirementNotFound", err)
	}
	if _, err := h.deps.proposePathHeads(ctx, proposeHeadsInput{SiteCode: "WH1", PathId: "pack"}); err == nil {
		t.Fatal("neither charge nor shiftId must be refused")
	}

	// A charge keeps the deprecated sizing even when a shiftId is named.
	out, err = h.deps.proposePathHeads(ctx, proposeHeadsInput{SiteCode: "WH1", ShiftId: "shift-1", PathId: "pack", Charge: chargeOf(100), PlannedRate: 30})
	if err != nil || out.ProposedHeads != 4 || out.RateSource != "" {
		t.Fatalf("charge path = %+v, %v; want 4 heads as before", out, err)
	}
}
