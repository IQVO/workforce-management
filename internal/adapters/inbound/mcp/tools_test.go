package mcp

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// fixedClock is a ports.Clock returning a fixed instant, so use-case timing
// is deterministic under test. The workforce clock adapter only ships a
// System clock, so the test supplies its own.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// unlimitedInstalledCapacity is a test double for
// ports.InstalledCapacityClient that always reports capacity as
// effectively unlimited, so tests focused on other behavior don't need
// to separately script the live-capacity fetch Feature C introduced.
type unlimitedInstalledCapacity struct{}

func (unlimitedInstalledCapacity) InstalledCapacity(_ context.Context, _ shared.PathId) (int, error) {
	return math.MaxInt32, nil
}

// harness builds Deps over in-memory repos, seeding through the real use
// cases (CommitShiftPlan, StartAssociateShift) with a fixed clock.
type harness struct {
	deps        Deps
	associates  *memory.AssociateRepo
	shiftPlans  *memory.ShiftPlanRepo
	assignments *memory.AssignmentRepo
	start       *usecases.StartAssociateShift
	commit      *usecases.CommitShiftPlan
	clock       fixedClock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	associates := memory.NewAssociateRepo()
	shiftPlans := memory.NewShiftPlanRepo()
	assignments := memory.NewAssignmentRepo()
	publisher := events.NewLogPublisher(nil)
	clk := fixedClock{now: base}
	const maxHours = 10.0

	return &harness{
		deps: Deps{
			GetStaffingGap:  &usecases.GetStaffingGap{ShiftPlans: shiftPlans, Assignments: assignments, Events: publisher, Clock: clk},
			ProposePathPlan: &usecases.ProposePathPlan{Events: publisher, Clock: clk},
			AssignLabor:     &usecases.AssignLabor{Associates: associates, Assignments: assignments, Events: publisher, Clock: clk, MaxHoursPerShift: maxHours},
		},
		associates:  associates,
		shiftPlans:  shiftPlans,
		assignments: assignments,
		start:       &usecases.StartAssociateShift{Associates: associates, Events: publisher, Clock: clk},
		commit:      &usecases.CommitShiftPlan{ShiftPlans: shiftPlans, Events: publisher, Clock: clk, InstalledCapacity: unlimitedInstalledCapacity{}, MaxHoursPerShift: maxHours},
		clock:       clk,
	}
}

// seedPlan commits a one-line shift plan for buildingId/shiftId with
// plannedHeads on pathId.
func (h *harness) seedPlan(t *testing.T, buildingId, shiftId, pathId string, plannedHeads int) {
	t.Helper()
	lines := []shiftplan.PathPlan{{
		PathId:       shared.PathId(pathId),
		PlannedHeads: plannedHeads,
		PlannedRate:  10,
		PlannedHours: 0,
	}}
	installed := map[shared.PathId]int{shared.PathId(pathId): plannedHeads + 10}
	if _, err := h.commit.Execute(context.Background(), buildingId, shiftId, lines, installed); err != nil {
		t.Fatalf("seedPlan: %v", err)
	}
}

// seedAssociate starts a shift for associateId holding the given certifications.
func (h *harness) seedAssociate(t *testing.T, associateId string, certs ...string) {
	t.Helper()
	cc := make([]shared.Certification, 0, len(certs))
	for _, c := range certs {
		cc = append(cc, shared.Certification(c))
	}
	if _, err := h.start.Execute(context.Background(), shared.AssociateId(associateId), cc); err != nil {
		t.Fatalf("seedAssociate: %v", err)
	}
}

func TestGetStaffingGap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedPlan(t, "B1", "S1", "pack", 3)
	// Put one associate on pack so activeHeads = 1, plannedHeads = 3.
	h.seedAssociate(t, "a1", "pack")
	if _, err := h.deps.AssignLabor.Execute(ctx, "a1", "pack"); err != nil {
		t.Fatalf("assign for seed: %v", err)
	}

	tests := []struct {
		name             string
		in               staffingGapInput
		wantErr          bool
		wantPlanned      int
		wantActive       int
		wantUnderstaffed bool
	}{
		{"understaffed pack", staffingGapInput{BuildingId: "B1", ShiftId: "S1", PathId: "pack"}, false, 3, 1, true},
		{"unknown path -> planned 0, not understaffed", staffingGapInput{BuildingId: "B1", ShiftId: "S1", PathId: "pick"}, false, 0, 0, false},
		{"missing buildingId rejected", staffingGapInput{BuildingId: "", ShiftId: "S1", PathId: "pack"}, true, 0, 0, false},
		{"missing shiftId rejected", staffingGapInput{BuildingId: "B1", ShiftId: "", PathId: "pack"}, true, 0, 0, false},
		{"missing pathId rejected", staffingGapInput{BuildingId: "B1", ShiftId: "S1", PathId: ""}, true, 0, 0, false},
		{"unknown plan errors (not found)", staffingGapInput{BuildingId: "NOPE", ShiftId: "S9", PathId: "pack"}, true, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := h.deps.getStaffingGap(ctx, tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.PlannedHeads != tc.wantPlanned || out.ActiveHeads != tc.wantActive || out.Understaffed != tc.wantUnderstaffed {
				t.Fatalf("gap = %+v, want planned=%d active=%d understaffed=%v", out, tc.wantPlanned, tc.wantActive, tc.wantUnderstaffed)
			}
			if out.PathId != tc.in.PathId || out.BuildingId != tc.in.BuildingId || out.ShiftId != tc.in.ShiftId {
				t.Fatalf("gap echo = %+v, want building/shift/path from input", out)
			}
		})
	}
}

func TestProposePathHeads(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		in        proposeHeadsInput
		wantErr   bool
		wantHeads int
	}{
		{"rounds up", proposeHeadsInput{BuildingId: "B1", PathId: "pack", Charge: 100, PlannedRate: 30}, false, 4},
		{"exact division", proposeHeadsInput{BuildingId: "B1", PathId: "pack", Charge: 90, PlannedRate: 30}, false, 3},
		{"zero charge -> zero heads", proposeHeadsInput{BuildingId: "B1", PathId: "pack", Charge: 0, PlannedRate: 30}, false, 0},
		{"missing buildingId rejected", proposeHeadsInput{BuildingId: "", PathId: "pack", Charge: 10, PlannedRate: 30}, true, 0},
		{"missing pathId rejected", proposeHeadsInput{BuildingId: "B1", PathId: "", Charge: 10, PlannedRate: 30}, true, 0},
		{"negative charge rejected", proposeHeadsInput{BuildingId: "B1", PathId: "pack", Charge: -1, PlannedRate: 30}, true, 0},
		{"zero rate rejected", proposeHeadsInput{BuildingId: "B1", PathId: "pack", Charge: 10, PlannedRate: 0}, true, 0},
		{"negative rate rejected", proposeHeadsInput{BuildingId: "B1", PathId: "pack", Charge: 10, PlannedRate: -5}, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := h.deps.proposePathHeads(ctx, tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.ProposedHeads != tc.wantHeads {
				t.Fatalf("proposedHeads = %d, want %d", out.ProposedHeads, tc.wantHeads)
			}
			if out.BuildingId != tc.in.BuildingId || out.PathId != tc.in.PathId {
				t.Fatalf("echo = %+v, want building/path from input", out)
			}
		})
	}
}

// assignLaborHappyPath pins the certified happy path: the assignment is
// created with a non-empty id echoing associate and path.
func assignLaborHappyPath(t *testing.T) {
	t.Helper()
	h := newHarness(t)
	h.seedAssociate(t, "a1", "pack")
	out, err := h.deps.assignLabor(context.Background(), assignLaborInput{AssociateId: "a1", PathId: "pack"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.AssociateId != "a1" || out.PathId != "pack" {
		t.Fatalf("unexpected output: %+v", out)
	}
	if out.AssignmentId == "" {
		t.Fatal("expected a non-empty assignment id")
	}
}

// assignLaborCertificationMismatch pins the certification-match invariant:
// assigning an associate to a path they are not certified for is rejected.
func assignLaborCertificationMismatch(t *testing.T) {
	t.Helper()
	h := newHarness(t)
	// Associate holds pick, not pack: assigning to pack must be rejected by
	// the certification-match invariant.
	h.seedAssociate(t, "a1", "pick")
	if _, err := h.deps.assignLabor(context.Background(), assignLaborInput{AssociateId: "a1", PathId: "pack"}); err == nil {
		t.Fatal("assigning an uncertified associate must be rejected")
	}
}

// assignLaborReassignment pins the reassignment flow: a second assignment
// to a different path is accepted, moves the associate, and leaves exactly
// one active assignment on the new path.
func assignLaborReassignment(t *testing.T) {
	t.Helper()
	h := newHarness(t)
	h.seedAssociate(t, "a1", "pack", "pick")
	if _, err := h.deps.assignLabor(context.Background(), assignLaborInput{AssociateId: "a1", PathId: "pack"}); err != nil {
		t.Fatalf("first assign failed: %v", err)
	}
	// Second assignment to a different path is accepted and moves the
	// associate; the single-active invariant is enforced by construction,
	// so this is a reassignment, not a rejection.
	out, err := h.deps.assignLabor(context.Background(), assignLaborInput{AssociateId: "a1", PathId: "pick"})
	if err != nil {
		t.Fatalf("reassignment should succeed: %v", err)
	}
	if out.PathId != "pick" {
		t.Fatalf("after reassignment active path = %q, want pick", out.PathId)
	}
	// Exactly one active assignment for this associate remains.
	la, err := h.assignments.FindByAssociateID(context.Background(), "a1")
	if err != nil {
		t.Fatalf("load assignment: %v", err)
	}
	activePath, ok := la.ActivePathId()
	if !ok || activePath != "pick" {
		t.Fatalf("want single active assignment on pick, got %q ok=%v", activePath, ok)
	}
}

// assignLaborUnknownAssociate pins the unknown-associate rejection.
func assignLaborUnknownAssociate(t *testing.T) {
	t.Helper()
	h := newHarness(t)
	if _, err := h.deps.assignLabor(context.Background(), assignLaborInput{AssociateId: "ghost", PathId: "pack"}); err == nil {
		t.Fatal("assigning an unknown associate must error")
	}
}

// assignLaborMissingArgs pins the argument-validation rejections: an empty
// associateId and an empty pathId are both rejected.
func assignLaborMissingArgs(t *testing.T) {
	t.Helper()
	h := newHarness(t)
	if _, err := h.deps.assignLabor(context.Background(), assignLaborInput{AssociateId: "", PathId: "pack"}); err == nil {
		t.Fatal("empty associateId must be rejected")
	}
	if _, err := h.deps.assignLabor(context.Background(), assignLaborInput{AssociateId: "a1", PathId: ""}); err == nil {
		t.Fatal("empty pathId must be rejected")
	}
}

func TestAssignLabor(t *testing.T) {
	t.Run("certified associate is assigned", func(t *testing.T) {
		assignLaborHappyPath(t)
	})

	t.Run("certification mismatch is rejected", func(t *testing.T) {
		assignLaborCertificationMismatch(t)
	})

	t.Run("re-assignment ends the prior active assignment (single active)", func(t *testing.T) {
		assignLaborReassignment(t)
	})

	t.Run("unknown associate is rejected", func(t *testing.T) {
		assignLaborUnknownAssociate(t)
	})

	t.Run("missing args are rejected", func(t *testing.T) {
		assignLaborMissingArgs(t)
	})
}

func TestParseStaffingURI(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		wantErr bool
		b, s, p string
	}{
		{"valid", "staffing://B1/S1/pack/gap", false, "B1", "S1", "pack"},
		{"wrong scheme", "queue://B1/S1/pack/gap", true, "", "", ""},
		{"missing gap suffix", "staffing://B1/S1/pack", true, "", "", ""},
		{"wrong suffix", "staffing://B1/S1/pack/status", true, "", "", ""},
		{"empty segment", "staffing://B1//pack/gap", true, "", "", ""},
		{"too many segments", "staffing://B1/S1/pack/extra/gap", true, "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, s, p, err := parseStaffingURI(tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if b != tc.b || s != tc.s || p != tc.p {
				t.Fatalf("parsed (%q,%q,%q), want (%q,%q,%q)", b, s, p, tc.b, tc.s, tc.p)
			}
		})
	}
}
