package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
)

// LabourRequirementRepo is a pgxpool-backed ports.LabourRequirements over the
// labour_requirements table (migration 000006, ADR 0043): the local copy of
// warehouse-planning's LabourPlanCommitted. Every statement joins the
// UnitOfWork transaction bound to ctx when there is one, so the consumer's
// claim and this upsert commit together.
type LabourRequirementRepo struct {
	pool *pgxpool.Pool
}

// NewLabourRequirementRepo constructs a LabourRequirementRepo backed by pool.
func NewLabourRequirementRepo(pool *pgxpool.Pool) *LabourRequirementRepo {
	return &LabourRequirementRepo{pool: pool}
}

// Find loads the requirement for key; found is false when no row exists.
func (r *LabourRequirementRepo) Find(ctx context.Context, key labourplan.Key) (labourplan.Requirement, bool, error) {
	req := labourplan.Requirement{Key: key}
	var source string
	err := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT planned_heads, rate_units_per_hour, hours, heads_source, version
		FROM labour_requirements WHERE site_code = $1 AND shift_id = $2 AND path_id = $3
	`, key.SiteCode, key.ShiftId, string(key.PathId)).Scan(&req.PlannedHeads, &req.RateUnitsPerHour, &req.Hours, &source, &req.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return labourplan.Requirement{}, false, nil
	}
	if err != nil {
		return labourplan.Requirement{}, false, err
	}
	req.HeadsSource = labourplan.HeadsSource(source)
	return req, true, nil
}

// Save upserts req. The WHERE clause is the version guard of ADR 0043
// decision 1 enforced in the store: an existing row is replaced only by a
// strictly greater version, so an equal or older write is a no-op even when
// two writers race past the use case's own check.
func (r *LabourRequirementRepo) Save(ctx context.Context, req labourplan.Requirement) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO labour_requirements (site_code, shift_id, path_id, planned_heads, rate_units_per_hour, hours, heads_source, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (site_code, shift_id, path_id) DO UPDATE SET
			planned_heads = EXCLUDED.planned_heads,
			rate_units_per_hour = EXCLUDED.rate_units_per_hour,
			hours = EXCLUDED.hours,
			heads_source = EXCLUDED.heads_source,
			version = EXCLUDED.version,
			updated_at = now()
		WHERE labour_requirements.version < EXCLUDED.version
	`, req.Key.SiteCode, req.Key.ShiftId, string(req.Key.PathId), req.PlannedHeads, req.RateUnitsPerHour, req.Hours, string(req.HeadsSource), req.Version)
	return err
}

// LabourPlanConsumedEvents is the labour-plan consumer's ports.ProcessedEvents
// claim over labour_plan_consumed_events (one row per CloudEvents id, the
// same shape as the analytics consumer's analytics_consumed_events). It joins
// the UnitOfWork bound to ctx, so a rolled-back apply also releases the claim
// and the redelivered message is applied again.
type LabourPlanConsumedEvents struct {
	pool *pgxpool.Pool
}

// NewLabourPlanConsumedEvents constructs the claim store over pool.
func NewLabourPlanConsumedEvents(pool *pgxpool.Pool) *LabourPlanConsumedEvents {
	return &LabourPlanConsumedEvents{pool: pool}
}

// MarkProcessed records eventId if absent, returning true iff this call newly
// recorded it.
func (c *LabourPlanConsumedEvents) MarkProcessed(ctx context.Context, eventId string) (bool, error) {
	tag, err := querierFrom(ctx, c.pool).Exec(ctx,
		`INSERT INTO labour_plan_consumed_events (event_id) VALUES ($1) ON CONFLICT (event_id) DO NOTHING`,
		eventId)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Compile-time assertions that both satisfy their ports.
var (
	_ ports.LabourRequirements = (*LabourRequirementRepo)(nil)
	_ ports.ProcessedEvents    = (*LabourPlanConsumedEvents)(nil)
)
