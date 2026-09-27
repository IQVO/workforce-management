package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// AssociateRepo is a pgxpool-backed ports.AssociateRepo.
type AssociateRepo struct {
	pool *pgxpool.Pool
}

// NewAssociateRepo constructs an AssociateRepo backed by pool.
func NewAssociateRepo(pool *pgxpool.Pool) *AssociateRepo {
	return &AssociateRepo{pool: pool}
}

// Save upserts a's current state, version-guarded against a concurrent
// writer (ADR 0021): on an existing row it only applies when the row's
// current version still matches a.Version(), and always advances the row
// by exactly one version. ports.ErrConcurrentModification is returned when
// the row exists but its version no longer matches -- the caller must
// re-fetch and retry, not blindly re-Save the same in-memory aggregate.
func (r *AssociateRepo) Save(ctx context.Context, a *associate.AssociateShift) error {
	certs := certsToStrings(a.Certifications())
	tag, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO associate_shift (associate_id, certifications, on_break, hours_logged, ended, version)
		VALUES ($1, $2, $3, $4, $5, 1)
		ON CONFLICT (associate_id) DO UPDATE SET
			certifications = EXCLUDED.certifications,
			on_break = EXCLUDED.on_break,
			hours_logged = EXCLUDED.hours_logged,
			ended = EXCLUDED.ended,
			version = associate_shift.version + 1
		WHERE associate_shift.version = $6
	`, string(a.AssociateId()), certs, a.IsOnBreak(), a.HoursLogged(), a.Ended(), a.Version())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// The row exists (this is the ON CONFLICT arm; a plain INSERT
		// into an empty slot always affects exactly 1 row) but its
		// version no longer matches what a was loaded at.
		return ports.ErrConcurrentModification
	}
	return nil
}

// FindByID loads the AssociateShift for id, or ports.ErrNotFound.
func (r *AssociateRepo) FindByID(ctx context.Context, id shared.AssociateId) (*associate.AssociateShift, error) {
	var certs []string
	var onBreak, ended bool
	var hoursLogged float64
	var version int

	row := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT certifications, on_break, hours_logged, ended, version
		FROM associate_shift WHERE associate_id = $1
	`, string(id))

	if err := row.Scan(&certs, &onBreak, &hoursLogged, &ended, &version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ports.ErrNotFound
		}
		return nil, err
	}

	return associate.Rehydrate(id, stringsToCerts(certs), onBreak, hoursLogged, ended, version), nil
}

func certsToStrings(certs []shared.Certification) []string {
	out := make([]string, len(certs))
	for i, c := range certs {
		out[i] = string(c)
	}
	return out
}

func stringsToCerts(strs []string) []shared.Certification {
	out := make([]shared.Certification, len(strs))
	for i, s := range strs {
		out[i] = shared.Certification(s)
	}
	return out
}
