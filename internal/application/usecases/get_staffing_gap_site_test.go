package usecases

import (
	"context"
	"testing"

	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// Site-scoped staffing gap (decision 6, ADR 0034): the optional canonical
// siteCode on StartAssociateShift and on the gap queries. Unscoped callers
// must behave exactly as before.

func startAt(t *testing.T, f *fixtures, id shared.AssociateId, site shared.SiteCode, certs ...shared.Certification) {
	t.Helper()
	uc := &StartAssociateShift{Associates: f.associates, Events: f.pub, Clock: f.clock}
	if _, err := uc.ExecuteAtSite(context.Background(), id, certs, site); err != nil {
		t.Fatalf("start %s at %q: %v", id, site, err)
	}
}

func assignTo(t *testing.T, f *fixtures, id shared.AssociateId, path shared.PathId) {
	t.Helper()
	uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if _, err := uc.Execute(context.Background(), id, path); err != nil {
		t.Fatalf("assign %s to %s: %v", id, path, err)
	}
}

func commitPackPlan(t *testing.T, f *fixtures, heads int) {
	t.Helper()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 10}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: heads, PlannedRate: 30, PlannedHours: 8}}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 10}); err != nil {
		t.Fatalf("commit plan: %v", err)
	}
}

// twoSitesSamePath: WH1 has 2 packers, WH2 has 1, and one legacy associate
// (no site) is also on pack. Plan wants 3 heads.
func twoSitesSamePath(t *testing.T) *fixtures {
	t.Helper()
	f := newFixtures()
	startAt(t, f, "wh1-a", "WH1", "pack")
	startAt(t, f, "wh1-b", "WH1", "pack")
	startAt(t, f, "wh2-a", "WH2", "pack")
	startAt(t, f, "legacy-a", "", "pack")
	for _, id := range []shared.AssociateId{"wh1-a", "wh1-b", "wh2-a", "legacy-a"} {
		assignTo(t, f, id, "pack")
	}
	commitPackPlan(t, f, 3)
	return f
}

func TestStartAssociateShift_ExecuteAtSite_PersistsSite(t *testing.T) {
	f := newFixtures()
	uc := &StartAssociateShift{Associates: f.associates, Events: f.pub, Clock: f.clock}

	shift, err := uc.ExecuteAtSite(context.Background(), "assoc-1", []shared.Certification{"pack"}, "WH1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shift.SiteCode() != "WH1" {
		t.Fatalf("expected returned shift at WH1, got %q", shift.SiteCode())
	}
	stored, _ := f.associates.FindByID(context.Background(), "assoc-1")
	if stored.SiteCode() != "WH1" {
		t.Fatalf("expected stored shift at WH1, got %q", stored.SiteCode())
	}
}

func TestStartAssociateShift_Execute_LeavesSiteUnknown(t *testing.T) {
	f := newFixtures()
	uc := &StartAssociateShift{Associates: f.associates, Events: f.pub, Clock: f.clock}
	if _, err := uc.Execute(context.Background(), "assoc-1", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stored, _ := f.associates.FindByID(context.Background(), "assoc-1")
	if stored.SiteCode() != "" {
		t.Fatalf("a call without siteCode must leave the site unknown, got %q", stored.SiteCode())
	}
}

// Restart is the documented upsert: the new roster entry replaces the old
// one entirely, site included (certifications behave the same way).
func TestStartAssociateShift_RestartReplacesSite(t *testing.T) {
	f := newFixtures()
	startAt(t, f, "assoc-1", "WH1")
	startAt(t, f, "assoc-1", "WH2")
	stored, _ := f.associates.FindByID(context.Background(), "assoc-1")
	if stored.SiteCode() != "WH2" {
		t.Fatalf("expected restart to move the associate to WH2, got %q", stored.SiteCode())
	}
}

func TestGetStaffingGap_Unscoped_CountsEveryBuilding(t *testing.T) {
	f := twoSitesSamePath(t)
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}

	gap, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Exactly as before: everyone, legacy included.
	if gap.ActiveHeads != 4 || gap.Understaffed || gap.SiteCode != "" {
		t.Fatalf("unscoped gap must stay fleet-wide, got %+v", gap)
	}
}

func TestGetStaffingGap_ExecuteForSite_CountsOnlyThatSite(t *testing.T) {
	f := twoSitesSamePath(t)
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}

	wh1, err := uc.ExecuteForSite(context.Background(), "bldg-1", "shift-1", "pack", "WH1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wh1.ActiveHeads != 2 || !wh1.Understaffed || wh1.SiteCode != "WH1" {
		t.Fatalf("WH1 must see only its 2 packers (3 planned), got %+v", wh1)
	}

	wh2, err := uc.ExecuteForSite(context.Background(), "bldg-1", "shift-1", "pack", "WH2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wh2.ActiveHeads != 1 || !wh2.Understaffed || wh2.SiteCode != "WH2" {
		t.Fatalf("WH2 must see only its 1 packer, got %+v", wh2)
	}
}

func TestGetStaffingGap_ExecuteForSite_LegacyNullSiteNeverCounted(t *testing.T) {
	f := newFixtures()
	startAt(t, f, "legacy-a", "", "pack")
	assignTo(t, f, "legacy-a", "pack")
	commitPackPlan(t, f, 1)
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}

	scoped, err := uc.ExecuteForSite(context.Background(), "bldg-1", "shift-1", "pack", "WH1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scoped.ActiveHeads != 0 || !scoped.Understaffed {
		t.Fatalf("a legacy (no-site) associate must not count at any site, got %+v", scoped)
	}
	unscoped, _ := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if unscoped.ActiveHeads != 1 || unscoped.Understaffed {
		t.Fatalf("a legacy associate must still count in unscoped queries, got %+v", unscoped)
	}
}

func TestGetStaffingGap_ExecuteForSite_BlankSiteIsUnscoped(t *testing.T) {
	f := twoSitesSamePath(t)
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}
	gap, err := uc.ExecuteForSite(context.Background(), "bldg-1", "shift-1", "pack", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gap.ActiveHeads != 4 || gap.SiteCode != "" {
		t.Fatalf("an empty siteCode must behave exactly like Execute, got %+v", gap)
	}
}

func pathUnderstaffedEvents(f *fixtures) []shared.PathUnderstaffed {
	var out []shared.PathUnderstaffed
	for _, e := range f.pub.Events() {
		if pu, ok := e.(shared.PathUnderstaffed); ok {
			out = append(out, pu)
		}
	}
	return out
}

func TestGetStaffingGap_ExecuteForSite_EventCarriesSite(t *testing.T) {
	f := twoSitesSamePath(t)
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}
	if _, err := uc.ExecuteForSite(context.Background(), "bldg-1", "shift-1", "pack", "WH1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	evts := pathUnderstaffedEvents(f)
	if len(evts) != 1 {
		t.Fatalf("expected 1 PathUnderstaffed, got %d", len(evts))
	}
	if evts[0].SiteCode != "WH1" || evts[0].ActiveHeads != 2 || evts[0].PlannedHeads != 3 {
		t.Fatalf("event must carry the scoped site and its count, got %+v", evts[0])
	}
}

func TestGetStaffingGap_Unscoped_EventCarriesNoSite(t *testing.T) {
	f := newFixtures()
	commitPackPlan(t, f, 3)
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}
	if _, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	evts := pathUnderstaffedEvents(f)
	if len(evts) != 1 || evts[0].SiteCode != "" {
		t.Fatalf("an unscoped gap must publish an event with no site, got %+v", evts)
	}
}

func TestGetStaffingGap_ExecuteAllForSite_ScopesEveryLine(t *testing.T) {
	f := twoSitesSamePath(t)
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}

	gaps, err := uc.ExecuteAllForSite(context.Background(), "bldg-1", "shift-1", "WH2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gaps) != 1 || gaps[0].ActiveHeads != 1 || gaps[0].SiteCode != "WH2" {
		t.Fatalf("expected the single pack line scoped to WH2, got %+v", gaps)
	}
	single, _ := uc.ExecuteForSite(context.Background(), "bldg-1", "shift-1", "pack", "WH2")
	if single != gaps[0] {
		t.Fatalf("list %+v diverges from single %+v", gaps[0], single)
	}

	all, err := uc.ExecuteAll(context.Background(), "bldg-1", "shift-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(all) != 1 || all[0].ActiveHeads != 4 || all[0].SiteCode != "" {
		t.Fatalf("unscoped list must stay fleet-wide, got %+v", all)
	}
}
