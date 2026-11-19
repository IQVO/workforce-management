//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/claudioed/workforce-management/internal/domain/labourplan"
)

// labourPool cleans the labour-plan copy and its claim table on the shared
// migrated pool from TestMain.
func labourPool(t *testing.T) *LabourRequirementRepo {
	t.Helper()
	pool := testPool(t)
	for _, table := range []string{"labour_requirements", "labour_plan_consumed_events"} {
		if _, err := pool.Exec(context.Background(), "DELETE FROM "+table); err != nil {
			t.Fatalf("clean %s: %v", table, err)
		}
	}
	return NewLabourRequirementRepo(pool)
}

func requirementAt(version, heads int) labourplan.Requirement {
	return labourplan.Requirement{
		Key:              labourplan.Key{SiteCode: "WH1", ShiftId: "2026-11-18-day", PathId: "PICK"},
		PlannedHeads:     heads,
		RateUnitsPerHour: 120,
		Hours:            8,
		HeadsSource:      labourplan.HeadsSourceForecast,
		Version:          version,
	}
}

func TestLabourRequirementRepo_FindMissingRowIsNotFound(t *testing.T) {
	repo := labourPool(t)
	_, found, err := repo.Find(context.Background(), requirementAt(1, 1).Key)
	if err != nil || found {
		t.Fatalf("Find on an empty copy = found %v, err %v; want not found, nil", found, err)
	}
}

// The store enforces the version guard on its own: only a strictly greater
// version replaces the row, an equal or older one is a no-op.
func TestLabourRequirementRepo_SaveAppliesOnlyAGreaterVersion(t *testing.T) {
	repo := labourPool(t)
	ctx := context.Background()
	for _, step := range []struct {
		save      labourplan.Requirement
		wantHeads int
		wantVer   int
	}{
		{requirementAt(2, 5), 5, 2},
		{requirementAt(3, 7), 7, 3},
		{requirementAt(3, 9), 7, 3}, // equal version: no-op
		{requirementAt(1, 2), 7, 3}, // older version: no-op
	} {
		if err := repo.Save(ctx, step.save); err != nil {
			t.Fatalf("Save v%d: %v", step.save.Version, err)
		}
		got, found, err := repo.Find(ctx, step.save.Key)
		if err != nil || !found {
			t.Fatalf("Find after v%d: found %v, err %v", step.save.Version, found, err)
		}
		if got.PlannedHeads != step.wantHeads || got.Version != step.wantVer || got.HeadsSource != labourplan.HeadsSourceForecast {
			t.Fatalf("after saving v%d the copy is %+v, want %d heads at v%d", step.save.Version, got, step.wantHeads, step.wantVer)
		}
	}
}

// The claim and the upsert share one transaction: a rolled-back unit of work
// releases the claim, so the redelivered message is applied again.
func TestLabourPlanConsumedEvents_ClaimRollsBackWithTheUnitOfWork(t *testing.T) {
	repo := labourPool(t)
	claims := NewLabourPlanConsumedEvents(mainPool)
	uow := NewUnitOfWork(mainPool)
	ctx := context.Background()
	errBoom := errors.New("boom")

	err := uow.Execute(ctx, func(ctx context.Context) error {
		if isNew, err := claims.MarkProcessed(ctx, "evt-1"); err != nil || !isNew {
			t.Fatalf("first claim = %v, %v", isNew, err)
		}
		if err := repo.Save(ctx, requirementAt(1, 4)); err != nil {
			t.Fatal(err)
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("Execute = %v, want the rollback cause", err)
	}
	if _, found, _ := repo.Find(ctx, requirementAt(1, 4).Key); found {
		t.Fatal("the rolled-back upsert is visible")
	}
	isNew, err := claims.MarkProcessed(ctx, "evt-1")
	if err != nil || !isNew {
		t.Fatalf("claim after rollback = %v, %v; want a fresh claim", isNew, err)
	}
	if again, err := claims.MarkProcessed(ctx, "evt-1"); err != nil || again {
		t.Fatalf("second committed claim = %v, %v; want a duplicate", again, err)
	}
}
