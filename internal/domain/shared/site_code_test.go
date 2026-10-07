package shared

import "testing"

func TestNewSiteCode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want SiteCode
	}{
		{"canonical code is kept as given", "WH1", SiteCode("WH1")},
		{"surrounding whitespace is trimmed", "  SIM1 ", SiteCode("SIM1")},
		{"case is NOT normalised (accepted as given)", "wh-1", SiteCode("wh-1")},
		{"empty means unscoped", "", SiteCode("")},
		{"blank means unscoped", "   ", SiteCode("")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NewSiteCode(tc.in)
			if got != tc.want {
				t.Fatalf("NewSiteCode(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got.IsUnscoped() != (tc.want == "") {
				t.Fatalf("IsUnscoped() = %v for %q", got.IsUnscoped(), got)
			}
		})
	}
}

func TestNewPathUnderstaffedAtSite_CarriesSiteCode(t *testing.T) {
	e := NewPathUnderstaffedAtSite(fixedTime, PathId("pack"), 10, 7, SiteCode("WH1"))

	if e.EventName() != "PathUnderstaffed" {
		t.Fatalf("expected EventName PathUnderstaffed, got %s", e.EventName())
	}
	if e.SiteCode != SiteCode("WH1") {
		t.Fatalf("expected SiteCode WH1, got %q", e.SiteCode)
	}
	if e.PathId != PathId("pack") || e.PlannedHeads != 10 || e.ActiveHeads != 7 {
		t.Fatalf("unexpected payload: %+v", e)
	}
}

func TestNewPathUnderstaffed_IsUnscoped(t *testing.T) {
	e := NewPathUnderstaffed(fixedTime, PathId("pack"), 10, 7)
	if e.SiteCode != "" {
		t.Fatalf("unscoped constructor must leave SiteCode empty, got %q", e.SiteCode)
	}
}
