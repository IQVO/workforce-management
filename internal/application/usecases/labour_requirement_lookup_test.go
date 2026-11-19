package usecases

import (
	"context"
	"errors"
	"testing"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// ADR 0043 decision 4: committing fewer heads than warehouse-planning
// requires is a legitimate supply decision; the requirement is only shown.
func TestCommitShiftPlan_NeverRefusesALineBelowTheRequirement(t *testing.T) {
	f := newFixtures()
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock,
		InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 10, "pick": 10}},
		Catalogue:         testCatalogue(), MaxHoursPerShift: 8,
		Requirements: seededCopy(t, labourRequirement(1, 6))}
	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 2, PlannedRate: 30, PlannedHours: 16},
		{PathId: "pick", PlannedHeads: 1, PlannedRate: 30, PlannedHours: 8},
	}
	sp, err := uc.Execute(context.Background(), "WH1", "shift-1", lines, map[shared.PathId]int{"pack": 10, "pick": 10})
	if err != nil {
		t.Fatalf("a commit below the requirement was refused: %v", err)
	}
	required, err := uc.RequiredHeads(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(required) != 1 || required["pack"] != 6 {
		t.Fatalf("required = %v, want only pack=6 (pick has no published row)", required)
	}

	none, err := (&CommitShiftPlan{}).RequiredHeads(context.Background(), sp)
	if err != nil || len(none) != 0 {
		t.Fatalf("no copy wired: %v, %v; want nothing shown", none, err)
	}
	broken := &CommitShiftPlan{Requirements: failingRequirements{LabourRequirementRepo: memory.NewLabourRequirementRepo(), findErr: errBoom}}
	if _, err := broken.RequiredHeads(context.Background(), sp); !errors.Is(err, errBoom) {
		t.Fatalf("find failure: err = %v", err)
	}
}

func TestGetStaffingGap_ShowsTheRequirementNextToThePlan(t *testing.T) {
	f := newFixtures()
	plan := shiftplan.Rehydrate("WH1", "shift-1", []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 2, PlannedRate: 30, PlannedHours: 16},
		{PathId: "pick", PlannedHeads: 1, PlannedRate: 30, PlannedHours: 8},
	})
	if err := f.shiftPlans.Save(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock,
		Requirements: seededCopy(t, labourRequirement(1, 6))}

	gaps, err := uc.ExecuteAll(context.Background(), "WH1", "shift-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gaps {
		switch g.PathId {
		case "pack":
			if g.RequiredHeads == nil || *g.RequiredHeads != 6 || g.PlannedHeads != 2 {
				t.Fatalf("pack gap = %+v, want 6 required next to 2 planned", g)
			}
		case "pick":
			if g.RequiredHeads != nil {
				t.Fatalf("pick has no published row, got required %d", *g.RequiredHeads)
			}
		}
	}

	broken := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock,
		Requirements: failingRequirements{LabourRequirementRepo: memory.NewLabourRequirementRepo(), findErr: errBoom}}
	if _, err := broken.Execute(context.Background(), "WH1", "shift-1", "pack"); !errors.Is(err, errBoom) {
		t.Fatalf("find failure: err = %v", err)
	}
}
