package associate

import (
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

func TestNewAssociateShiftAtSite_RecordsSite(t *testing.T) {
	a := NewAssociateShiftAtSite("assoc-1", []shared.Certification{"pack"}, shared.SiteCode("WH1"), time.Now())

	if a.SiteCode() != shared.SiteCode("WH1") {
		t.Fatalf("expected site WH1, got %q", a.SiteCode())
	}
	if len(a.PullEvents()) != 1 {
		t.Fatal("starting a shift at a site must still raise exactly AssociateShiftStarted")
	}
}

func TestNewAssociateShift_HasNoSite(t *testing.T) {
	a := NewAssociateShift("assoc-1", nil, time.Now())
	if a.SiteCode() != "" {
		t.Fatalf("legacy constructor must leave the site unknown, got %q", a.SiteCode())
	}
}

func TestRehydrate_CarriesSite(t *testing.T) {
	scoped := RehydrateAtSite("assoc-1", nil, false, 0, false, 3, shared.SiteCode("WH2"))
	if scoped.SiteCode() != shared.SiteCode("WH2") {
		t.Fatalf("expected site WH2, got %q", scoped.SiteCode())
	}
	legacy := Rehydrate("assoc-2", nil, false, 0, false, 1)
	if legacy.SiteCode() != "" {
		t.Fatalf("legacy row must rehydrate with no site, got %q", legacy.SiteCode())
	}
}

func TestSite_SurvivesLifecycleTransitions(t *testing.T) {
	now := time.Now()
	a := NewAssociateShiftAtSite("assoc-1", nil, shared.SiteCode("WH1"), now)
	if err := a.Certify("pack", now); err != nil {
		t.Fatal(err)
	}
	if err := a.StartBreak(now); err != nil {
		t.Fatal(err)
	}
	a.EndShift(now)
	if a.SiteCode() != shared.SiteCode("WH1") {
		t.Fatalf("site must not change over the shift lifecycle, got %q", a.SiteCode())
	}
}
