//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// Site-scoped staffing gap against a real Postgres (decision 6, ADR 0034).

func saveShiftAt(t *testing.T, repo *AssociateRepo, id string, site shared.SiteCode) *associate.AssociateShift {
	t.Helper()
	shift := associate.NewAssociateShiftAtSite(shared.AssociateId(id), []shared.Certification{"pack"}, site, time.Now())
	shift.PullEvents()
	if err := repo.Save(context.Background(), shift); err != nil {
		t.Fatalf("save shift %s: %v", id, err)
	}
	return shift
}

func assignToPath(t *testing.T, repo *AssignmentRepo, id string, path shared.PathId) {
	t.Helper()
	la := assignment.NewLaborAssignment(shared.AssociateId(id))
	if err := la.Assign(path, true, time.Now()); err != nil {
		t.Fatalf("assign %s: %v", id, err)
	}
	la.PullEvents()
	if err := repo.Save(context.Background(), la); err != nil {
		t.Fatalf("save assignment %s: %v", id, err)
	}
}

func TestAssociateRepo_SiteCodeRoundTrip(t *testing.T) {
	pool := testPool(t)
	repo := NewAssociateRepo(pool)
	ctx := context.Background()

	saveShiftAt(t, repo, "assoc-wh1", "WH1")
	saveShiftAt(t, repo, "assoc-none", "")

	scoped, err := repo.FindByID(ctx, "assoc-wh1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if scoped.SiteCode() != "WH1" {
		t.Fatalf("expected site WH1, got %q", scoped.SiteCode())
	}

	none, err := repo.FindByID(ctx, "assoc-none")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if none.SiteCode() != "" {
		t.Fatalf("expected no site, got %q", none.SiteCode())
	}

	// An unknown site is stored as NULL, never as the empty string.
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT site_code IS NULL FROM associate_shift WHERE associate_id = 'assoc-none'`).Scan(&isNull); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !isNull {
		t.Fatal("a shift started without a site must persist site_code as NULL")
	}
}

// Restart (upsert) replaces the site, version-guarded like every other field.
func TestAssociateRepo_RestartMovesSite(t *testing.T) {
	pool := testPool(t)
	repo := NewAssociateRepo(pool)
	ctx := context.Background()

	first := saveShiftAt(t, repo, "assoc-1", "WH1")

	restart := associate.NewAssociateShiftAtSite("assoc-1", nil, "WH2", time.Now())
	restart.SetVersion(first.Version())
	restart.PullEvents()
	if err := repo.Save(ctx, restart); err != nil {
		t.Fatalf("restart save: %v", err)
	}

	loaded, err := repo.FindByID(ctx, "assoc-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if loaded.SiteCode() != "WH2" {
		t.Fatalf("expected the restart to move the associate to WH2, got %q", loaded.SiteCode())
	}
}

// The headline scenario: two sites, same path.
func TestAssignmentRepo_CountActiveByPathAtSite_TwoSitesSamePath(t *testing.T) {
	pool := testPool(t)
	associates := NewAssociateRepo(pool)
	assignments := NewAssignmentRepo(pool)
	ctx := context.Background()

	saveShiftAt(t, associates, "wh1-a", "WH1")
	saveShiftAt(t, associates, "wh1-b", "WH1")
	saveShiftAt(t, associates, "wh2-a", "WH2")
	for _, id := range []string{"wh1-a", "wh1-b", "wh2-a"} {
		assignToPath(t, assignments, id, "pack")
	}
	// A different path at WH1 must not leak into pack's count.
	saveShiftAt(t, associates, "wh1-stow", "WH1")
	assignToPath(t, assignments, "wh1-stow", "stow")

	for _, tc := range []struct {
		site shared.SiteCode
		path shared.PathId
		want int
	}{
		{"WH1", "pack", 2},
		{"WH2", "pack", 1},
		{"WH1", "stow", 1},
		{"WH2", "stow", 0},
		{"NOPE", "pack", 0},
		{"", "pack", 0}, // empty is never an accidental fleet-wide count
	} {
		got, err := assignments.CountActiveByPathAtSite(ctx, tc.path, tc.site)
		if err != nil {
			t.Fatalf("count %s@%q: %v", tc.path, tc.site, err)
		}
		if got != tc.want {
			t.Fatalf("CountActiveByPathAtSite(%s, %q) = %d, want %d", tc.path, tc.site, got, tc.want)
		}
	}

	// The unscoped count is untouched: every site.
	all, err := assignments.CountActiveByPath(ctx, "pack")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if all != 3 {
		t.Fatalf("unscoped CountActiveByPath(pack) = %d, want 3", all)
	}
}

// Legacy rows (written before migration 000005: no site_code) count only in
// unscoped queries.
func TestAssignmentRepo_CountActiveByPathAtSite_LegacyNullSite(t *testing.T) {
	pool := testPool(t)
	assignments := NewAssignmentRepo(pool)
	ctx := context.Background()

	// Insert the way the pre-000005 schema/code did: no site_code column.
	if _, err := pool.Exec(ctx, `INSERT INTO associate_shift (associate_id, certifications, on_break, hours_logged, ended, version)
		VALUES ('legacy-1', '{pack}', FALSE, 0, FALSE, 1)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	assignToPath(t, assignments, "legacy-1", "pack")

	scoped, err := assignments.CountActiveByPathAtSite(ctx, "pack", "WH1")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if scoped != 0 {
		t.Fatalf("a legacy NULL-site associate must not count at any site, got %d", scoped)
	}
	unscoped, err := assignments.CountActiveByPath(ctx, "pack")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if unscoped != 1 {
		t.Fatalf("a legacy associate must still count unscoped, got %d", unscoped)
	}
}

// Only ACTIVE shifts count at a site: an ended shift never does.
func TestAssignmentRepo_CountActiveByPathAtSite_IgnoresEndedShift(t *testing.T) {
	pool := testPool(t)
	associates := NewAssociateRepo(pool)
	assignments := NewAssignmentRepo(pool)
	ctx := context.Background()

	shift := saveShiftAt(t, associates, "wh1-a", "WH1")
	assignToPath(t, assignments, "wh1-a", "pack")

	loaded, err := associates.FindByID(ctx, "wh1-a")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	_ = shift
	loaded.EndShift(time.Now())
	loaded.PullEvents()
	if err := associates.Save(ctx, loaded); err != nil {
		t.Fatalf("save ended shift: %v", err)
	}

	got, err := assignments.CountActiveByPathAtSite(ctx, "pack", "WH1")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 0 {
		t.Fatalf("an ended shift must not count at its site, got %d", got)
	}
}
