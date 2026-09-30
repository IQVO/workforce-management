package assignment

import (
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

func TestAssign_FirstAssignmentRaisesLaborAssigned(t *testing.T) {
	now := time.Now()
	la := NewLaborAssignment("assoc-1")

	if err := la.Assign("pack", true, now); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := la.PullEvents()
	if len(events) != 1 || events[0].EventName() != "LaborAssigned" {
		t.Fatalf("expected LaborAssigned event, got %+v", events)
	}
	pathId, active := la.ActivePathId()
	if !active || pathId != "pack" {
		t.Fatalf("expected active assignment to pack, got %v active=%v", pathId, active)
	}
}

// TestAssign_RejectsMissingCertification is a Definition-of-Done named
// failing-path test: assignment without the required certification must be
// rejected.
func TestAssign_RejectsMissingCertification(t *testing.T) {
	now := time.Now()
	la := NewLaborAssignment("assoc-1")

	err := la.Assign("hazmat", false, now)
	if err != ErrCertificationRequired {
		t.Fatalf("expected ErrCertificationRequired, got %v", err)
	}
	if la.IsActive() {
		t.Fatal("expected no active assignment after rejected assign")
	}
}

// TestAssign_SecondAssignmentEndsPriorAndRaisesReassigned is a
// Definition-of-Done named failing-path test in spirit: it proves the
// "exactly one ACTIVE assignment per associate" invariant holds by
// construction — assigning again always ends the previous active interval
// rather than creating a second active one.
func TestAssign_SecondAssignmentEndsPriorAndRaisesReassigned(t *testing.T) {
	start := time.Now()
	la := NewLaborAssignment("assoc-1")
	if err := la.Assign("pack", true, start); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	la.PullEvents()

	later := start.Add(2 * time.Hour)
	if err := la.Assign("stow", true, later); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := la.PullEvents()
	if len(events) != 1 || events[0].EventName() != "LaborReassigned" {
		t.Fatalf("expected LaborReassigned event, got %+v", events)
	}

	pathId, active := la.ActivePathId()
	if !active || pathId != "stow" {
		t.Fatalf("expected single active assignment to stow, got %v active=%v", pathId, active)
	}

	history := la.History()
	if len(history) != 1 || history[0].PathId != "pack" || history[0].End == nil {
		t.Fatalf("expected pack interval closed in history, got %+v", history)
	}
}

func TestEndActive_ClosesActiveAssignment(t *testing.T) {
	start := time.Now()
	la := NewLaborAssignment("assoc-1")
	_ = la.Assign("pack", true, start)
	la.PullEvents()

	end := start.Add(4 * time.Hour)
	closed, ok := la.EndActive(end)
	if !ok {
		t.Fatal("expected an active interval to close")
	}
	if closed.PathId != "pack" || closed.Hours(end) != 4 {
		t.Fatalf("expected 4 hour pack interval, got %+v", closed)
	}
	if la.IsActive() {
		t.Fatal("expected no active assignment after EndActive")
	}
}

func TestEndActive_NoOpWhenNoneActive(t *testing.T) {
	la := NewLaborAssignment("assoc-1")
	_, ok := la.EndActive(time.Now())
	if ok {
		t.Fatal("expected no active interval to close")
	}
}

// TestCloseActive_WithoutActiveAssignmentIsNoOp covers closeActive's own
// nil-active guard. Both exported callers (Assign, EndActive) only invoke it
// with an active interval present, so the guard is exercised directly here —
// it must be a strict no-op: no history entry, no active interval, no panic.
func TestCloseActive_WithoutActiveAssignmentIsNoOp(t *testing.T) {
	la := NewLaborAssignment("assoc-1")

	la.closeActive(time.Now())

	if la.IsActive() {
		t.Fatal("expected no active assignment after no-op closeActive")
	}
	if len(la.History()) != 0 {
		t.Fatalf("expected no history entries after no-op closeActive, got %d", len(la.History()))
	}
}

func TestInterval_HoursOnActiveInterval(t *testing.T) {
	start := time.Now()
	iv := Interval{PathId: "pack", Start: start}

	hours := iv.Hours(start.Add(2 * time.Hour))
	if hours != 2 {
		t.Fatalf("expected 2 hours for still-active interval, got %v", hours)
	}
}

func TestActivePathId_NoneActive(t *testing.T) {
	la := NewLaborAssignment("assoc-1")

	pathId, active := la.ActivePathId()
	if active || pathId != "" {
		t.Fatalf("expected no active assignment, got %v active=%v", pathId, active)
	}
}

func TestActiveInterval_NoneActive(t *testing.T) {
	la := NewLaborAssignment("assoc-1")

	iv, ok := la.ActiveInterval()
	if ok || iv != (Interval{}) {
		t.Fatalf("expected no active interval, got %+v ok=%v", iv, ok)
	}
}

func TestActiveInterval_ReturnsCopyOfActive(t *testing.T) {
	start := time.Now()
	la := NewLaborAssignment("assoc-1")
	if err := la.Assign("pack", true, start); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	iv, ok := la.ActiveInterval()
	if !ok {
		t.Fatal("expected an active interval")
	}
	if iv.PathId != "pack" || !iv.Start.Equal(start) || iv.End != nil {
		t.Fatalf("unexpected active interval: %+v", iv)
	}
}

func TestRehydrate_PreservesState(t *testing.T) {
	start := time.Now()
	end := start.Add(time.Hour)
	history := []Interval{{PathId: "stow", Start: start, End: &end}}
	active := &Interval{PathId: "pack", Start: end}

	la := Rehydrate("assoc-1", active, history, 3)
	if la.AssociateId() != shared.AssociateId("assoc-1") {
		t.Fatalf("unexpected associate id: %v", la.AssociateId())
	}
	if la.Version() != 3 {
		t.Fatalf("expected Rehydrate to preserve version 3, got %d", la.Version())
	}
	pathId, ok := la.ActivePathId()
	if !ok || pathId != "pack" {
		t.Fatalf("expected active pack assignment, got %v ok=%v", pathId, ok)
	}
	if len(la.History()) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(la.History()))
	}
}
