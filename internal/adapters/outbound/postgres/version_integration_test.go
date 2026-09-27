//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// Optimistic-concurrency (ADR 0021) repo-level tests: a stale-version Save
// fails with ports.ErrConcurrentModification, a current-version Save
// succeeds and advances the version by exactly one, and a real two-
// goroutine race on the SAME loaded aggregate resolves to exactly one
// winner. All three run against a real testcontainers Postgres — never an
// external DATABASE_URL, never t.Skip.

func TestAssociateRepo_Save_StaleVersionFails(t *testing.T) {
	pool := outboxDB(t)
	repo := postgres.NewAssociateRepo(pool)
	ctx := context.Background()

	shift := associate.NewAssociateShift("assoc-1", []shared.Certification{"pack"}, time.Now())
	shift.PullEvents()
	if err := repo.Save(ctx, shift); err != nil {
		t.Fatalf("initial save: %v", err)
	}
	if shift.Version() != 1 {
		t.Fatalf("expected fresh aggregate to start at version 1, got %d", shift.Version())
	}

	// Two independent loads of the same row -- simulating two requests
	// that each read-modify-write without knowing about the other.
	first, err := repo.FindByID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("load first: %v", err)
	}
	second, err := repo.FindByID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("load second: %v", err)
	}

	if err := first.Certify("hazmat", time.Now()); err != nil {
		t.Fatalf("certify first: %v", err)
	}
	if err := repo.Save(ctx, first); err != nil {
		t.Fatalf("save first: %v", err)
	}
	if first.Version() != 1 {
		t.Fatalf("Save must not mutate the caller's in-memory version; got %d", first.Version())
	}

	// second still holds the OLD version (1); the row is now at version 2.
	if err := second.Certify("stow", time.Now()); err != nil {
		t.Fatalf("certify second: %v", err)
	}
	err = repo.Save(ctx, second)
	if !errors.Is(err, ports.ErrConcurrentModification) {
		t.Fatalf("expected ErrConcurrentModification on stale-version save, got %v", err)
	}

	// The first writer's change survived; the second's was rejected, not
	// silently clobbered.
	loaded, err := repo.FindByID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !loaded.HasCertification("hazmat") {
		t.Fatal("expected the first writer's certification to persist")
	}
	if loaded.HasCertification("stow") {
		t.Fatal("expected the second (stale) writer's certification to have been rejected, not applied")
	}
}

func TestAssociateRepo_Save_CurrentVersionSucceedsAndIncrements(t *testing.T) {
	pool := outboxDB(t)
	repo := postgres.NewAssociateRepo(pool)
	ctx := context.Background()

	shift := associate.NewAssociateShift("assoc-1", nil, time.Now())
	shift.PullEvents()
	if err := repo.Save(ctx, shift); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	loaded, err := repo.FindByID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Version() != 1 {
		t.Fatalf("expected version 1 after first save, got %d", loaded.Version())
	}
	if err := loaded.Certify("pack", time.Now()); err != nil {
		t.Fatalf("certify: %v", err)
	}
	if err := repo.Save(ctx, loaded); err != nil {
		t.Fatalf("save at current version: %v", err)
	}

	reloaded, err := repo.FindByID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Version() != 2 {
		t.Fatalf("expected version to advance to 2, got %d", reloaded.Version())
	}
	if !reloaded.HasCertification("pack") {
		t.Fatal("expected the certification to persist")
	}
}

func TestAssignmentRepo_Save_StaleVersionFails(t *testing.T) {
	pool := outboxDB(t)
	repo := postgres.NewAssignmentRepo(pool)
	ctx := context.Background()

	start := time.Now()
	la := assignment.NewLaborAssignment("assoc-1")
	if err := la.Assign("pack", true, start); err != nil {
		t.Fatalf("assign: %v", err)
	}
	la.PullEvents()
	if err := repo.Save(ctx, la); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	first, err := repo.FindByAssociateID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("load first: %v", err)
	}
	second, err := repo.FindByAssociateID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("load second: %v", err)
	}

	if err := first.Assign("stow", true, start.Add(time.Hour)); err != nil {
		t.Fatalf("reassign first: %v", err)
	}
	if err := repo.Save(ctx, first); err != nil {
		t.Fatalf("save first: %v", err)
	}

	if err := second.Assign("pick", true, start.Add(2*time.Hour)); err != nil {
		t.Fatalf("reassign second: %v", err)
	}
	err = repo.Save(ctx, second)
	if !errors.Is(err, ports.ErrConcurrentModification) {
		t.Fatalf("expected ErrConcurrentModification on stale-version save, got %v", err)
	}

	loaded, err := repo.FindByAssociateID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	pathId, active := loaded.ActivePathId()
	if !active || pathId != "stow" {
		t.Fatalf("expected the first writer's reassignment (stow) to persist, got active=%v path=%v", active, pathId)
	}
}

func TestAssignmentRepo_Save_CurrentVersionSucceedsAndIncrements(t *testing.T) {
	pool := outboxDB(t)
	repo := postgres.NewAssignmentRepo(pool)
	ctx := context.Background()

	la := assignment.NewLaborAssignment("assoc-1")
	if err := la.Assign("pack", true, time.Now()); err != nil {
		t.Fatalf("assign: %v", err)
	}
	la.PullEvents()
	if err := repo.Save(ctx, la); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	loaded, err := repo.FindByAssociateID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Version() != 1 {
		t.Fatalf("expected version 1, got %d", loaded.Version())
	}
	if err := loaded.Assign("stow", true, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if err := repo.Save(ctx, loaded); err != nil {
		t.Fatalf("save at current version: %v", err)
	}

	reloaded, err := repo.FindByAssociateID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Version() != 2 {
		t.Fatalf("expected version to advance to 2, got %d", reloaded.Version())
	}
}

// TestAssociateRepo_ConcurrentSaves_ExactlyOneWinner is the real end-to-end
// proof the race is closed: two goroutines each load the SAME row, mutate
// their own in-memory copy, and Save concurrently. Exactly one must
// succeed; the other must observe ErrConcurrentModification. A sequential
// simulation (like the stale-version tests above) proves the SQL guard
// works, but not that it survives genuinely concurrent commits -- this
// test is what actually exercises that.
func TestAssociateRepo_ConcurrentSaves_ExactlyOneWinner(t *testing.T) {
	pool := outboxDB(t)
	repo := postgres.NewAssociateRepo(pool)
	ctx := context.Background()

	shift := associate.NewAssociateShift("assoc-race", nil, time.Now())
	shift.PullEvents()
	if err := repo.Save(ctx, shift); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	loadedA, err := repo.FindByID(ctx, "assoc-race")
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	loadedB, err := repo.FindByID(ctx, "assoc-race")
	if err != nil {
		t.Fatalf("load B: %v", err)
	}
	if err := loadedA.Certify("hazmat", time.Now()); err != nil {
		t.Fatalf("mutate A: %v", err)
	}
	if err := loadedB.Certify("stow", time.Now()); err != nil {
		t.Fatalf("mutate B: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = repo.Save(ctx, loadedA)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = repo.Save(ctx, loadedB)
	}()
	close(start)
	wg.Wait()

	successes, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ports.ErrConcurrentModification):
			conflicts++
		default:
			t.Fatalf("unexpected error from concurrent Save: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("expected exactly one success and one conflict, got successes=%d conflicts=%d (errs=%v)", successes, conflicts, errs)
	}

	final, err := repo.FindByID(ctx, "assoc-race")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if final.Version() != 2 {
		t.Fatalf("expected exactly one version increment despite two concurrent attempts, got %d", final.Version())
	}
	// Exactly one of the two certifications must have landed -- never
	// both (that would mean the guard let a lost update through) and
	// never neither.
	got := 0
	if final.HasCertification("hazmat") {
		got++
	}
	if final.HasCertification("stow") {
		got++
	}
	if got != 1 {
		t.Fatalf("expected exactly one certification to have landed, got %d (hazmat=%v stow=%v)", got, final.HasCertification("hazmat"), final.HasCertification("stow"))
	}
}
