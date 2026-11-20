package usecases

import (
	"context"
	"errors"
	"testing"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

func seededCopy(t *testing.T, reqs ...labourplan.Requirement) *memory.LabourRequirementRepo {
	t.Helper()
	repo := memory.NewLabourRequirementRepo()
	for _, r := range reqs {
		if err := repo.Save(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// ADR 0043 decision 2: the heads are the published requirement, the rate is
// warehouse-planning's, the source is labour-plan and no idle-share trim
// applies even when the observed idle share is far above the threshold.
func TestProposePathPlan_FromLabourPlanIsThePublishedRequirementUntrimmed(t *testing.T) {
	f := newFixtures()
	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{}
	uc := &ProposePathPlan{Events: pub, Clock: f.clock, UnitOfWork: uow,
		IdleShare:    &fakeIdleShareClient{share: 0.9},
		Requirements: seededCopy(t, labourRequirement(3, 6))}

	heads, rate, source, err := uc.ExecuteFromLabourPlan(context.Background(), "WH1", "shift-1", "pack")
	if err != nil {
		t.Fatal(err)
	}
	if heads != 6 || rate != 30 || source != RateSourceLabourPlan {
		t.Fatalf("got %d heads at %v from %q, want 6 at 30 from labour-plan", heads, rate, source)
	}
	if pub.events != 1 || len(pub.inScope) != 1 || !pub.inScope[0] || uow.committed != 1 {
		t.Fatalf("ShiftPlanProposed must be published once inside the unit of work: %+v, uow %+v", pub, uow)
	}
}

func TestProposePathPlan_FromLabourPlanWithoutARowIsNotFoundNeverZero(t *testing.T) {
	f := newFixtures()
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, Requirements: seededCopy(t, labourRequirement(1, 6))}
	for _, key := range [][3]string{{"WH2", "shift-1", "pack"}, {"WH1", "shift-2", "pack"}, {"WH1", "shift-1", "pick"}} {
		heads, _, _, err := uc.ExecuteFromLabourPlan(context.Background(), key[0], key[1], shared.PathId(key[2]))
		if !errors.Is(err, labourplan.ErrRequirementNotFound) || heads != 0 {
			t.Fatalf("%v: heads %d, err %v; want ErrRequirementNotFound", key, heads, err)
		}
	}
}

func TestProposePathPlan_FromLabourPlanFailures(t *testing.T) {
	f := newFixtures()
	if _, _, _, err := (&ProposePathPlan{Events: f.pub, Clock: f.clock}).ExecuteFromLabourPlan(context.Background(), "WH1", "shift-1", "pack"); !errors.Is(err, ErrProposeNoLabourPlanCopy) {
		t.Fatalf("no copy wired: err = %v", err)
	}
	broken := failingRequirements{LabourRequirementRepo: memory.NewLabourRequirementRepo(), findErr: errBoom}
	if _, _, _, err := (&ProposePathPlan{Events: f.pub, Clock: f.clock, Requirements: broken}).ExecuteFromLabourPlan(context.Background(), "WH1", "shift-1", "pack"); !errors.Is(err, errBoom) {
		t.Fatalf("find failure: err = %v", err)
	}
	uc := &ProposePathPlan{Events: &scopedPublisher{err: errBoom}, Clock: f.clock, Requirements: seededCopy(t, labourRequirement(1, 2))}
	if _, _, _, err := uc.ExecuteFromLabourPlan(context.Background(), "WH1", "shift-1", "pack"); !errors.Is(err, errBoom) {
		t.Fatalf("publish failure: err = %v", err)
	}
}
