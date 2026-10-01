package usecases

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// scopeKey marks a context as "inside the unit of work" so the fakes can
// assert every Save/Publish happened within the scope, never outside it.
type scopeKey struct{}

// recordingUnitOfWork is a ports.UnitOfWork fake that (a) tags the ctx it
// hands to fn, (b) counts how many scopes were opened, and (c) reports
// whether each scope committed (fn returned nil) or rolled back.
type recordingUnitOfWork struct {
	opened     int
	committed  int
	rolledBack int
	beginErr   error
}

func (u *recordingUnitOfWork) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	if u.beginErr != nil {
		return u.beginErr
	}
	u.opened++
	err := fn(context.WithValue(ctx, scopeKey{}, true))
	if err != nil {
		u.rolledBack++
		return err
	}
	u.committed++
	return nil
}

func inScope(ctx context.Context) bool {
	v, _ := ctx.Value(scopeKey{}).(bool)
	return v
}

// scopedPublisher records, per Publish call, whether it ran inside a scope
// and how many events it carried; err makes every Publish fail.
type scopedPublisher struct {
	inScope []bool
	events  int
	err     error
}

func (p *scopedPublisher) Publish(ctx context.Context, evts ...shared.DomainEvent) error {
	p.inScope = append(p.inScope, inScope(ctx))
	if p.err != nil {
		return p.err
	}
	p.events += len(evts)
	return nil
}

// scopedAssociates / scopedAssignments / scopedShiftPlans record whether
// each Save ran inside the scope, delegating to the wrapped (in-memory)
// repo for everything else.
type scopedSaves struct{ saves []bool }

func (s *scopedSaves) record(ctx context.Context) { s.saves = append(s.saves, inScope(ctx)) }

type scopedAssociates struct {
	ports.AssociateRepo
	scopedSaves
}

func (r *scopedAssociates) Save(ctx context.Context, a *associate.AssociateShift) error {
	r.record(ctx)
	return r.AssociateRepo.Save(ctx, a)
}

type scopedAssignments struct {
	ports.AssignmentRepo
	scopedSaves
}

func (r *scopedAssignments) Save(ctx context.Context, la *assignment.LaborAssignment) error {
	r.record(ctx)
	return r.AssignmentRepo.Save(ctx, la)
}

type scopedShiftPlans struct {
	ports.ShiftPlanRepo
	scopedSaves
}

func (r *scopedShiftPlans) Save(ctx context.Context, sp *shiftplan.ShiftPlan) error {
	r.record(ctx)
	return r.ShiftPlanRepo.Save(ctx, sp)
}

func allTrue(bs []bool) bool {
	for _, b := range bs {
		if !b {
			return false
		}
	}
	return true
}

func assertOneCommittedScope(t *testing.T, uow *recordingUnitOfWork, pub *scopedPublisher, saves ...[]bool) {
	t.Helper()
	if uow.opened != 1 || uow.committed != 1 || uow.rolledBack != 0 {
		t.Fatalf("expected exactly one committed scope, got opened=%d committed=%d rolledBack=%d", uow.opened, uow.committed, uow.rolledBack)
	}
	if len(pub.inScope) != 1 || !pub.inScope[0] {
		t.Fatalf("expected exactly one Publish inside the unit of work, got %v", pub.inScope)
	}
	for i, s := range saves {
		if len(s) == 0 || !allTrue(s) {
			t.Fatalf("expected every Save of repo %d inside the unit of work, got %v", i, s)
		}
	}
}

func TestStartAssociateShift_SaveAndPublishRunInsideOneUnitOfWork(t *testing.T) {
	f := newFixtures()
	assoc := &scopedAssociates{AssociateRepo: f.associates}
	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{}
	uc := &StartAssociateShift{Associates: assoc, Events: pub, Clock: f.clock, UnitOfWork: uow}

	if _, err := uc.Execute(context.Background(), "assoc-1", []shared.Certification{"pack"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertOneCommittedScope(t, uow, pub, assoc.saves)
}

func TestStartAssociateShift_PublishFailureRollsBack(t *testing.T) {
	f := newFixtures()
	pub := &scopedPublisher{err: errors.New("outbox insert failed")}
	uow := &recordingUnitOfWork{}
	uc := &StartAssociateShift{Associates: f.associates, Events: pub, Clock: f.clock, UnitOfWork: uow}

	_, err := uc.Execute(context.Background(), "assoc-1", nil)
	if err == nil || err.Error() != "outbox insert failed" {
		t.Fatalf("expected the publish error to propagate, got %v", err)
	}
	if uow.rolledBack != 1 || uow.committed != 0 {
		t.Fatalf("expected the scope to roll back, got committed=%d rolledBack=%d", uow.committed, uow.rolledBack)
	}
}

func TestStartAssociateShift_UnitOfWorkBeginFailurePropagates(t *testing.T) {
	f := newFixtures()
	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{beginErr: errors.New("begin failed")}
	uc := &StartAssociateShift{Associates: f.associates, Events: pub, Clock: f.clock, UnitOfWork: uow}

	if _, err := uc.Execute(context.Background(), "assoc-1", nil); err == nil || err.Error() != "begin failed" {
		t.Fatalf("expected begin error, got %v", err)
	}
	if len(pub.inScope) != 0 {
		t.Fatal("expected nothing published when the unit of work cannot begin")
	}
}

func TestStartAssociateShift_NilUnitOfWorkStillSavesAndPublishes(t *testing.T) {
	f := newFixtures()
	pub := &scopedPublisher{}
	uc := &StartAssociateShift{Associates: f.associates, Events: pub, Clock: f.clock}

	if _, err := uc.Execute(context.Background(), "assoc-1", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pub.inScope) != 1 || pub.inScope[0] {
		t.Fatalf("expected one publish outside any scope, got %v", pub.inScope)
	}
	if _, err := f.associates.FindByID(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("expected the associate persisted without a unit of work: %v", err)
	}
}

func TestCertifyAssociate_SaveAndPublishRunInsideOneUnitOfWork(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	assoc := &scopedAssociates{AssociateRepo: f.associates}
	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{}
	uc := &CertifyAssociate{Associates: assoc, Events: pub, Clock: f.clock, UnitOfWork: uow}

	if err := uc.Execute(context.Background(), "assoc-1", "hazmat"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertOneCommittedScope(t, uow, pub, assoc.saves)
}

func TestCertifyAssociate_PublishFailureRollsBack(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	uow := &recordingUnitOfWork{}
	uc := &CertifyAssociate{Associates: f.associates, Events: &scopedPublisher{err: errBoom}, Clock: f.clock, UnitOfWork: uow}

	if err := uc.Execute(context.Background(), "assoc-1", "hazmat"); !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	if uow.rolledBack != 1 || uow.committed != 0 {
		t.Fatalf("expected a rollback, got committed=%d rolledBack=%d", uow.committed, uow.rolledBack)
	}
}

func TestStartBreak_EndBreak_RunInsideOneUnitOfWorkEach(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	assoc := &scopedAssociates{AssociateRepo: f.associates}

	pubStart, uowStart := &scopedPublisher{}, &recordingUnitOfWork{}
	start := &StartBreak{Associates: assoc, Events: pubStart, Clock: f.clock, UnitOfWork: uowStart}
	if err := start.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("start break: %v", err)
	}
	assertOneCommittedScope(t, uowStart, pubStart, assoc.saves)

	assoc.saves = nil
	pubEnd, uowEnd := &scopedPublisher{}, &recordingUnitOfWork{}
	end := &EndBreak{Associates: assoc, Events: pubEnd, Clock: f.clock, UnitOfWork: uowEnd}
	if err := end.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("end break: %v", err)
	}
	assertOneCommittedScope(t, uowEnd, pubEnd, assoc.saves)
}

func TestStartBreak_EndBreak_PublishFailureRollsBack(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")

	uow := &recordingUnitOfWork{}
	start := &StartBreak{Associates: f.associates, Events: &scopedPublisher{err: errBoom}, Clock: f.clock, UnitOfWork: uow}
	if err := start.Execute(context.Background(), "assoc-1"); !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	if uow.rolledBack != 1 {
		t.Fatalf("expected a rollback, got %+v", uow)
	}

	// The in-memory repo has no real rollback (it stores the aggregate
	// pointer), so the associate is already on break: EndBreak has
	// something to end without further setup.
	uow = &recordingUnitOfWork{}
	end := &EndBreak{Associates: f.associates, Events: &scopedPublisher{err: errBoom}, Clock: f.clock, UnitOfWork: uow}
	if err := end.Execute(context.Background(), "assoc-1"); !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	if uow.rolledBack != 1 {
		t.Fatalf("expected a rollback, got %+v", uow)
	}
}

func TestCommitShiftPlan_SaveAndPublishRunInsideOneUnitOfWork(t *testing.T) {
	f := newFixtures()
	plans := &scopedShiftPlans{ShiftPlanRepo: f.shiftPlans}
	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{}
	uc := &CommitShiftPlan{ShiftPlans: plans, Events: pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 10}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8, UnitOfWork: uow}

	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40}}
	if _, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 10}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertOneCommittedScope(t, uow, pub, plans.saves)
}

func TestCommitShiftPlan_PublishFailureRollsBack(t *testing.T) {
	f := newFixtures()
	uow := &recordingUnitOfWork{}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: &scopedPublisher{err: errBoom}, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 10}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8, UnitOfWork: uow}

	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40}}
	if _, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 10}); !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	if uow.rolledBack != 1 || uow.committed != 0 {
		t.Fatalf("expected a rollback, got %+v", uow)
	}
}

func TestCommitShiftPlan_RejectedPlanOpensNoUnitOfWork(t *testing.T) {
	f := newFixtures()
	uow := &recordingUnitOfWork{}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: &scopedPublisher{}, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 1}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8, UnitOfWork: uow}

	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40}}
	if _, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 10}); err == nil {
		t.Fatal("expected the over-capacity plan to be rejected")
	}
	if uow.opened != 0 {
		t.Fatalf("a rejected commit must not open a unit of work, got opened=%d", uow.opened)
	}
}

func TestAssignLabor_BothSavesAndPublishRunInsideOneUnitOfWork(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack", "stow")
	assoc := &scopedAssociates{AssociateRepo: f.associates}
	assign := &scopedAssignments{AssignmentRepo: f.assignments}
	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{}
	uc := &AssignLabor{Associates: assoc, Assignments: assign, Events: pub, Clock: f.clock, MaxHoursPerShift: 8, UnitOfWork: uow}

	// First assignment: no prior interval, so only the assignment is saved.
	if _, err := uc.Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	assertOneCommittedScope(t, uow, pub, assign.saves)
	if len(assoc.saves) != 0 {
		t.Fatalf("first assignment must not touch the associate, got saves=%v", assoc.saves)
	}

	// Reassignment closes the prior interval and logs hours: BOTH repos are
	// saved, and both must be inside the same (second) scope.
	*uow, *pub, assign.saves = recordingUnitOfWork{}, scopedPublisher{}, nil
	f.clock.now = f.clock.now.Add(2 * time.Hour)
	if _, err := uc.Execute(context.Background(), "assoc-1", "stow"); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	assertOneCommittedScope(t, uow, pub, assoc.saves, assign.saves)
}

func TestAssignLabor_PublishFailureRollsBack(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")
	uow := &recordingUnitOfWork{}
	uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: &scopedPublisher{err: errBoom}, Clock: f.clock, MaxHoursPerShift: 8, UnitOfWork: uow}

	if _, err := uc.Execute(context.Background(), "assoc-1", "pack"); !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	if uow.rolledBack != 1 || uow.committed != 0 {
		t.Fatalf("expected a rollback, got %+v", uow)
	}
}

func TestAssignLabor_RejectedAssignmentOpensNoUnitOfWork(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")
	uow := &recordingUnitOfWork{}
	uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: &scopedPublisher{}, Clock: f.clock, MaxHoursPerShift: 8, UnitOfWork: uow}

	if _, err := uc.Execute(context.Background(), "assoc-1", "hazmat"); err == nil {
		t.Fatal("expected the uncertified assignment to be rejected")
	}
	if uow.opened != 0 {
		t.Fatalf("a rejected assignment must not open a unit of work, got opened=%d", uow.opened)
	}
}

func TestEndAssociateShift_BothSavesAndPublishRunInsideOneUnitOfWork(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")
	if _, err := (&AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: &scopedPublisher{}, Clock: f.clock, MaxHoursPerShift: 8}).Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("setup assign: %v", err)
	}
	assoc := &scopedAssociates{AssociateRepo: f.associates}
	assign := &scopedAssignments{AssignmentRepo: f.assignments}
	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{}
	f.clock.now = f.clock.now.Add(time.Hour)
	uc := &EndAssociateShift{Associates: assoc, Assignments: assign, Events: pub, Clock: f.clock, MaxHoursPerShift: 8, UnitOfWork: uow}

	if err := uc.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertOneCommittedScope(t, uow, pub, assoc.saves, assign.saves)
}

func TestEndAssociateShift_NoActiveAssignment_OnlyAssociateSavedInsideScope(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	assoc := &scopedAssociates{AssociateRepo: f.associates}
	assign := &scopedAssignments{AssignmentRepo: f.assignments}
	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{}
	uc := &EndAssociateShift{Associates: assoc, Assignments: assign, Events: pub, Clock: f.clock, MaxHoursPerShift: 8, UnitOfWork: uow}

	if err := uc.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertOneCommittedScope(t, uow, pub, assoc.saves)
	if len(assign.saves) != 0 {
		t.Fatalf("no assignment to close, so none must be saved, got %v", assign.saves)
	}
}

func TestEndAssociateShift_PublishFailureRollsBack(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	uow := &recordingUnitOfWork{}
	uc := &EndAssociateShift{Associates: f.associates, Assignments: f.assignments, Events: &scopedPublisher{err: errBoom}, Clock: f.clock, MaxHoursPerShift: 8, UnitOfWork: uow}

	if err := uc.Execute(context.Background(), "assoc-1"); !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	if uow.rolledBack != 1 || uow.committed != 0 {
		t.Fatalf("expected a rollback, got %+v", uow)
	}
}

func TestProposePathPlan_PublishRunsInsideOneUnitOfWork(t *testing.T) {
	f := newFixtures()
	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{}
	uc := &ProposePathPlan{Events: pub, Clock: f.clock, UnitOfWork: uow}

	if _, _, _, _, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertOneCommittedScope(t, uow, pub)

	uow = &recordingUnitOfWork{}
	uc = &ProposePathPlan{Events: &scopedPublisher{err: errBoom}, Clock: f.clock, UnitOfWork: uow}
	if _, _, _, _, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 10); !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	if uow.rolledBack != 1 {
		t.Fatalf("expected a rollback, got %+v", uow)
	}
}

func TestGetStaffingGap_PublishRunsInsideOneUnitOfWorkOnlyWhenUnderstaffed(t *testing.T) {
	f := newFixtures()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: &scopedPublisher{}, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 2, PlannedRate: 30, PlannedHours: 16}}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{}
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: pub, Clock: f.clock, UnitOfWork: uow}
	gap, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if err != nil || !gap.Understaffed {
		t.Fatalf("expected an understaffed gap, got %+v err=%v", gap, err)
	}
	assertOneCommittedScope(t, uow, pub)

	// Fully staffed: no event, no scope.
	for _, id := range []shared.AssociateId{"a1", "a2"} {
		setupCertifiedAssociate(t, f, id, "pack")
		if _, err := (&AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: &scopedPublisher{}, Clock: f.clock, MaxHoursPerShift: 8}).Execute(context.Background(), id, "pack"); err != nil {
			t.Fatalf("setup assign %s: %v", id, err)
		}
	}
	uow = &recordingUnitOfWork{}
	uc.UnitOfWork = uow
	gap, err = uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if err != nil || gap.Understaffed {
		t.Fatalf("expected a fully staffed gap, got %+v err=%v", gap, err)
	}
	if uow.opened != 0 {
		t.Fatalf("a staffed path raises no event and must open no scope, got opened=%d", uow.opened)
	}

	uow = &recordingUnitOfWork{}
	failing := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: &scopedPublisher{err: errBoom}, Clock: f.clock, UnitOfWork: uow}
	if _, err := failing.Execute(context.Background(), "bldg-1", "shift-2", "pack"); err == nil {
		t.Fatal("expected a missing plan to error before any scope")
	}
	if uow.opened != 0 {
		t.Fatalf("a failed lookup must open no scope, got opened=%d", uow.opened)
	}
}
