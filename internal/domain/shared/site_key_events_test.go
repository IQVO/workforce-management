package shared

import "testing"

// The deprecated BuildingId and the canonical SiteCode are one value on the
// plan events (ADR 0035).
func TestShiftPlanEvents_SiteCodeIsTheBuildingIdValue(t *testing.T) {
	if got := NewShiftPlanProposed(fixedTime, "WH1", PathId("pack"), 5, 42.5).SiteCode(); got != "WH1" {
		t.Fatalf("ShiftPlanProposed.SiteCode() = %q, want WH1", got)
	}
	if got := NewShiftPlanCommitted(fixedTime, "WH1", "shift-1").SiteCode(); got != "WH1" {
		t.Fatalf("ShiftPlanCommitted.SiteCode() = %q, want WH1", got)
	}
}

func TestResolveGapLookup(t *testing.T) {
	tests := []struct {
		name        string
		site, bldg  string
		wantKey     string
		wantScope   SiteCode
		wantMissing bool
	}{
		{"siteCode only: key and scope", "WH1", "", "WH1", "WH1", false},
		{"legacy buildingId only: unscoped", "", "bldg-1", "bldg-1", "", false},
		{"both equal: key and scope", "WH1", "WH1", "WH1", "WH1", false},
		{"both different: ADR 0034 as before (building keys, site scopes)", "WH1", "bldg-1", "bldg-1", "WH1", false},
		{"neither", "", "", "", "", true},
		{"blank siteCode and buildingId", "  ", " ", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, scope, err := ResolveGapLookup(tc.site, tc.bldg)
			if tc.wantMissing {
				if err != ErrMissingSiteKey {
					t.Fatalf("err = %v, want ErrMissingSiteKey", err)
				}
				return
			}
			if err != nil || key != tc.wantKey || scope != tc.wantScope {
				t.Fatalf("got (%q,%q,%v), want (%q,%q,nil)", key, scope, err, tc.wantKey, tc.wantScope)
			}
		})
	}
}
