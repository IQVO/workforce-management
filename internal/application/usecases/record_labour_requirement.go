package usecases

import (
	"context"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
)

// RecordLabourRequirement applies one LabourPlanCommitted from
// warehouse-planning to the local copy (ADR 0043 decision 1). The claim on the
// CloudEvents id and the version-guarded upsert run in ONE unit of work: a
// failure rolls both back, so the redelivered message is applied again, and a
// committed claim makes every redelivery a no-op.
type RecordLabourRequirement struct {
	Requirements ports.LabourRequirements
	Processed    ports.ProcessedEvents
	// UnitOfWork brackets the claim and the upsert (ADR 0016); nil runs them
	// back to back (the in-memory configuration).
	UnitOfWork ports.UnitOfWork
}

// Execute validates r, claims eventId and stores r when it supersedes the
// stored row. applied reports whether the copy changed: false for a
// duplicate id and for an equal or older version. A requirement that breaks
// the contract returns an error wrapping labourplan.ErrInvalidRequirement
// before anything is claimed.
func (uc *RecordLabourRequirement) Execute(ctx context.Context, eventId string, r labourplan.Requirement) (applied bool, err error) {
	if err := r.Validate(); err != nil {
		return false, err
	}
	err = atomically(ctx, uc.UnitOfWork, func(ctx context.Context) error {
		isNew, err := uc.Processed.MarkProcessed(ctx, eventId)
		if err != nil || !isNew {
			return err
		}
		stored, found, err := uc.Requirements.Find(ctx, r.Key)
		if err != nil {
			return err
		}
		if !r.Supersedes(stored, found) {
			return nil
		}
		if err := uc.Requirements.Save(ctx, r); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}
