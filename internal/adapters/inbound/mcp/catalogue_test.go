package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
)

// testCatalogue mirrors the fleet's declared paths (ADR-0013): pack and
// pick families, matched case-insensitively by prefix.
func testCatalogue() *pathcatalog.Catalogue {
	return pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
		{Id: "PICK", MatchPrefix: "pick", RequiredCapabilities: []string{"pick"}},
	})
}

// ADR-0013 applies on the MCP surface exactly as on REST: a path id outside
// the catalogue is rejected before it reaches any use case, for the read and
// the write tools alike.
func TestTools_RejectPathIdsOutsideTheCatalogue(t *testing.T) {
	h := newHarness(t)
	h.deps.Catalogue = testCatalogue()
	h.seedAssociate(t, "a1", "hazmat")
	ctx := context.Background()

	t.Run("get_staffing_gap", func(t *testing.T) {
		_, err := h.deps.getStaffingGap(ctx, staffingGapInput{BuildingId: "B1", ShiftId: "S1", PathId: "hazmat"})
		if !errors.Is(err, pathcatalog.ErrUnknownPath) {
			t.Fatalf("err = %v, want ErrUnknownPath", err)
		}
	})
	t.Run("propose_path_heads", func(t *testing.T) {
		_, err := h.deps.proposePathHeads(ctx, proposeHeadsInput{BuildingId: "B1", PathId: "hazmat", Charge: 10, PlannedRate: 5})
		if !errors.Is(err, pathcatalog.ErrUnknownPath) {
			t.Fatalf("err = %v, want ErrUnknownPath", err)
		}
	})
	t.Run("assign_labor", func(t *testing.T) {
		_, err := h.deps.assignLabor(ctx, assignLaborInput{AssociateId: "a1", PathId: "hazmat"})
		if !errors.Is(err, pathcatalog.ErrUnknownPath) {
			t.Fatalf("err = %v, want ErrUnknownPath", err)
		}
	})
}

// A catalogue-valid id (case-insensitive prefix family) still reaches the
// use case.
func TestTools_AcceptCatalogueValidPathIds(t *testing.T) {
	h := newHarness(t)
	h.deps.Catalogue = testCatalogue()
	h.seedAssociate(t, "a1", "pick-zone-a")

	view, err := h.deps.assignLabor(context.Background(), assignLaborInput{AssociateId: "a1", PathId: "pick-zone-a"})
	if err != nil {
		t.Fatalf("assignLabor(pick-zone-a): %v", err)
	}
	if view.PathId != "pick-zone-a" {
		t.Fatalf("PathId = %q, want pick-zone-a", view.PathId)
	}
}
