package usecases

import (
	"context"
	"errors"
	"testing"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
)

func labourRequirement(version, heads int) labourplan.Requirement {
	return labourplan.Requirement{
		Key:              labourplan.Key{SiteCode: "WH1", ShiftId: "shift-1", PathId: "pack"},
		PlannedHeads:     heads,
		RateUnitsPerHour: 30,
		Hours:            8,
		HeadsSource:      labourplan.HeadsSourceForecast,
		Version:          version,
	}
}

type failingRequirements struct {
	*memory.LabourRequirementRepo
	findErr, saveErr error
}

func (f failingRequirements) Find(ctx context.Context, key labourplan.Key) (labourplan.Requirement, bool, error) {
	if f.findErr != nil {
		return labourplan.Requirement{}, false, f.findErr
	}
	return f.LabourRequirementRepo.Find(ctx, key)
}

func (f failingRequirements) Save(ctx context.Context, r labourplan.Requirement) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	return f.LabourRequirementRepo.Save(ctx, r)
}

type failingClaims struct{ err error }

func (f failingClaims) MarkProcessed(context.Context, string) (bool, error) { return false, f.err }

func TestRecordLabourRequirement_AppliesTheVersionGuardAndDedupesOnId(t *testing.T) {
	copyRepo := memory.NewLabourRequirementRepo()
	uow := &recordingUnitOfWork{}
	uc := &RecordLabourRequirement{Requirements: copyRepo, Processed: memory.NewProcessedEvents(), UnitOfWork: uow}
	ctx := context.Background()

	steps := []struct {
		id          string
		req         labourplan.Requirement
		wantApplied bool
		wantHeads   int
	}{
		{"evt-1", labourRequirement(1, 4), true, 4},
		{"evt-1", labourRequirement(9, 9), false, 4}, // duplicate id: no-op whatever it carries
		{"evt-2", labourRequirement(2, 6), true, 6},
		{"evt-3", labourRequirement(2, 7), false, 6}, // equal version: no-op
		{"evt-4", labourRequirement(1, 1), false, 6}, // older version: no-op
	}
	for _, s := range steps {
		applied, err := uc.Execute(ctx, s.id, s.req)
		if err != nil || applied != s.wantApplied {
			t.Fatalf("%s v%d: applied %v, err %v; want applied %v", s.id, s.req.Version, applied, err, s.wantApplied)
		}
		got, _, _ := copyRepo.Find(ctx, s.req.Key)
		if got.PlannedHeads != s.wantHeads {
			t.Fatalf("after %s the copy holds %d heads, want %d", s.id, got.PlannedHeads, s.wantHeads)
		}
	}
	if uow.opened != len(steps) || uow.rolledBack != 0 {
		t.Fatalf("unit of work opened %d (rolled back %d), want one committed scope per message", uow.opened, uow.rolledBack)
	}
}

func TestRecordLabourRequirement_RefusesAnInvalidRequirementBeforeClaiming(t *testing.T) {
	claims := memory.NewProcessedEvents()
	uc := &RecordLabourRequirement{Requirements: memory.NewLabourRequirementRepo(), Processed: claims}
	if _, err := uc.Execute(context.Background(), "evt-bad", labourRequirement(0, 1)); !errors.Is(err, labourplan.ErrInvalidRequirement) {
		t.Fatalf("Execute = %v, want ErrInvalidRequirement", err)
	}
	if isNew, _ := claims.MarkProcessed(context.Background(), "evt-bad"); !isNew {
		t.Fatal("an invalid requirement claimed its id")
	}
}

func TestRecordLabourRequirement_FailuresRollTheScopeBack(t *testing.T) {
	cases := map[string]*RecordLabourRequirement{
		"claim fails": {Requirements: memory.NewLabourRequirementRepo(), Processed: failingClaims{err: errBoom}},
		"find fails":  {Requirements: failingRequirements{LabourRequirementRepo: memory.NewLabourRequirementRepo(), findErr: errBoom}, Processed: memory.NewProcessedEvents()},
		"save fails":  {Requirements: failingRequirements{LabourRequirementRepo: memory.NewLabourRequirementRepo(), saveErr: errBoom}, Processed: memory.NewProcessedEvents()},
	}
	for name, uc := range cases {
		t.Run(name, func(t *testing.T) {
			uow := &recordingUnitOfWork{}
			uc.UnitOfWork = uow
			applied, err := uc.Execute(context.Background(), "evt-1", labourRequirement(1, 3))
			if !errors.Is(err, errBoom) || applied {
				t.Fatalf("Execute = applied %v, err %v; want the failure", applied, err)
			}
			if uow.rolledBack != 1 {
				t.Fatalf("rolled back %d scopes, want 1", uow.rolledBack)
			}
		})
	}
}
