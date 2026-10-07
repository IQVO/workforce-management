package mcp

import (
	"context"
	"testing"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// TestGetStaffingGap_SiteScoped covers the optional siteCode on the
// get_staffing_gap tool (ADR 0034): absent = fleet-wide as before; present =
// only associates at that site.
func TestGetStaffingGap_SiteScoped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedPlan(t, "B1", "S1", "pack", 3)
	for id, site := range map[string]shared.SiteCode{"a1": "WH1", "a2": "WH1", "a3": "WH2", "a4": ""} {
		if _, err := h.start.ExecuteAtSite(ctx, shared.AssociateId(id), []shared.Certification{"pack"}, site); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if _, err := h.deps.AssignLabor.Execute(ctx, shared.AssociateId(id), "pack"); err != nil {
			t.Fatalf("assign %s: %v", id, err)
		}
	}

	tests := []struct {
		name       string
		siteCode   string
		wantActive int
		wantSite   string
	}{
		{"unscoped counts everyone", "", 4, ""},
		{"WH1", "WH1", 2, "WH1"},
		{"WH2", "WH2", 1, "WH2"},
		{"unknown site counts nobody", "ZZ9", 0, "ZZ9"},
		{"blank is unscoped", "   ", 4, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := h.deps.getStaffingGap(ctx, staffingGapInput{BuildingId: "B1", ShiftId: "S1", PathId: "pack", SiteCode: tc.siteCode})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.ActiveHeads != tc.wantActive || out.SiteCode != tc.wantSite {
				t.Fatalf("got active=%d site=%q, want active=%d site=%q", out.ActiveHeads, out.SiteCode, tc.wantActive, tc.wantSite)
			}
		})
	}
}
