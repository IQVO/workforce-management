package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// AssignmentRepo is a pgxpool-backed ports.AssignmentRepo.
type AssignmentRepo struct {
	pool *pgxpool.Pool
}

// NewAssignmentRepo constructs an AssignmentRepo backed by pool.
func NewAssignmentRepo(pool *pgxpool.Pool) *AssignmentRepo {
	return &AssignmentRepo{pool: pool}
}

// Save upserts la's active assignment and appends any newly closed history
// entries, version-guarded against a concurrent writer (ADR 0021): the
// active-assignment row's update only applies when its current version
// still matches la.Version(), and always advances by exactly one version
// on success. ports.ErrConcurrentModification is returned when the row
// exists but its version no longer matches -- the caller must re-fetch and
// retry. The multi-statement write joins the UnitOfWork transaction bound
// to ctx when there is one, else runs in its own (ADR 0016).
func (r *AssignmentRepo) Save(ctx context.Context, la *assignment.LaborAssignment) error {
	tx, commit, rollback, err := beginOrJoin(ctx, r.pool)
	if err != nil {
		return err
	}
	defer func() { _ = rollback(ctx) }()

	var activePathId *string
	var activeStart *time.Time
	if iv, ok := la.ActiveInterval(); ok {
		p := string(iv.PathId)
		activePathId = &p
		start := iv.Start
		activeStart = &start
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO labor_assignment (associate_id, active_path_id, active_start, version)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (associate_id) DO UPDATE SET
			active_path_id = EXCLUDED.active_path_id,
			active_start = EXCLUDED.active_start,
			version = labor_assignment.version + 1
		WHERE labor_assignment.version = $4
	`, string(la.AssociateId()), activePathId, activeStart, la.Version())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// The row exists (this is the ON CONFLICT arm; a plain INSERT
		// into an empty slot always affects exactly 1 row) but its
		// version no longer matches what la was loaded at.
		return ports.ErrConcurrentModification
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM labor_assignment_history WHERE associate_id = $1
	`, string(la.AssociateId())); err != nil {
		return err
	}

	for _, iv := range la.History() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO labor_assignment_history (associate_id, path_id, interval_start, interval_end)
			VALUES ($1, $2, $3, $4)
		`, string(la.AssociateId()), string(iv.PathId), iv.Start, iv.End); err != nil {
			return err
		}
	}

	return commit(ctx)
}

// FindByAssociateID loads the LaborAssignment for id, or ports.ErrNotFound.
func (r *AssignmentRepo) FindByAssociateID(ctx context.Context, id shared.AssociateId) (*assignment.LaborAssignment, error) {
	var activePathId *string
	var activeStart *time.Time
	var version int

	row := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT active_path_id, active_start, version FROM labor_assignment WHERE associate_id = $1
	`, string(id))
	if err := row.Scan(&activePathId, &activeStart, &version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ports.ErrNotFound
		}
		return nil, err
	}

	var active *assignment.Interval
	if activePathId != nil && activeStart != nil {
		active = &assignment.Interval{PathId: shared.PathId(*activePathId), Start: *activeStart}
	}

	rows, err := querierFrom(ctx, r.pool).Query(ctx, `
		SELECT path_id, interval_start, interval_end
		FROM labor_assignment_history WHERE associate_id = $1
		ORDER BY id ASC
	`, string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []assignment.Interval
	for rows.Next() {
		var pathId string
		var iv assignment.Interval
		var end time.Time
		if err := rows.Scan(&pathId, &iv.Start, &end); err != nil {
			return nil, err
		}
		iv.PathId = shared.PathId(pathId)
		iv.End = &end
		history = append(history, iv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return assignment.Rehydrate(id, active, history, version), nil
}

// CountActiveByPath counts how many associates currently have an active
// assignment to pathId.
func (r *AssignmentRepo) CountActiveByPath(ctx context.Context, pathId shared.PathId) (int, error) {
	var count int
	err := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT COUNT(*) FROM labor_assignment WHERE active_path_id = $1
	`, string(pathId)).Scan(&count)
	return count, err
}
