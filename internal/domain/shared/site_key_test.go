package shared

import (
	"errors"
	"testing"
)

// TestResolveSiteKey pins the convergence rule of ADR 0035: siteCode is the
// canonical name of the ShiftPlan key, buildingId is a deprecated alias with
// the same value. At least one is required; both present and equal is fine;
// both present and different is a conflict.
func TestResolveSiteKey(t *testing.T) {
	tests := []struct {
		name       string
		siteCode   string
		buildingId string
		want       string
		wantErr    error
	}{
		{"siteCode only", "WH1", "", "WH1", nil},
		{"buildingId only (deprecated alias)", "", "bldg-1", "bldg-1", nil},
		{"both present and equal", "WH1", "WH1", "WH1", nil},
		{"both present and different", "WH1", "bldg-1", "", ErrConflictingSiteAndBuilding},
		{"neither", "", "", "", ErrMissingSiteKey},
		{"blank is absent: siteCode blank, buildingId set", "   ", "bldg-1", "bldg-1", nil},
		{"blank is absent: both blank", "  ", " ", "", ErrMissingSiteKey},
		{"whitespace is trimmed before comparing", " WH1 ", "WH1", "WH1", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveSiteKey(tc.siteCode, tc.buildingId)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ResolveSiteKey(%q,%q) error = %v, want %v", tc.siteCode, tc.buildingId, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("ResolveSiteKey(%q,%q) = %q, want %q", tc.siteCode, tc.buildingId, got, tc.want)
			}
		})
	}
}
