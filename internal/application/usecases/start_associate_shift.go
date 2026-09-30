// Package usecases implements the Workforce Management use cases. Each use
// case is one struct depending only on domain packages and application
// ports — never on a concrete adapter.
package usecases

import (
	"context"
	"errors"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// StartAssociateShift starts a shift roster entry for an associate.
type StartAssociateShift struct {
	Associates ports.AssociateRepo
	Events     ports.EventPublisher
	Clock      ports.Clock
	// UnitOfWork brackets Save + Publish atomically (ADR 0016); nil = none.
	UnitOfWork ports.UnitOfWork
}

// Execute starts the associate's shift with the given certifications.
// This endpoint is documented as idempotent by associate id (see
// apis/openapi.yaml): a second call for the same associate restarts the
// roster entry rather than erroring. Because Save is now version-guarded
// (ADR 0021), restarting must carry over any EXISTING row's version --
// otherwise the second call's Save would be rejected as a stale write
// against a fresh aggregate that always starts at version 1.
func (uc *StartAssociateShift) Execute(ctx context.Context, associateId shared.AssociateId, certifications []shared.Certification) (*associate.AssociateShift, error) {
	shift := associate.NewAssociateShift(associateId, certifications, uc.Clock.Now())

	existing, err := uc.Associates.FindByID(ctx, associateId)
	switch {
	case err == nil:
		shift.SetVersion(existing.Version())
	case errors.Is(err, ports.ErrNotFound):
		// No existing roster entry: shift keeps NewAssociateShift's
		// fresh version (1), which is what a first-time INSERT expects.
	default:
		return nil, err
	}

	err = atomically(ctx, uc.UnitOfWork, func(ctx context.Context) error {
		if err := uc.Associates.Save(ctx, shift); err != nil {
			return err
		}
		return uc.Events.Publish(ctx, shift.PullEvents()...)
	})
	if err != nil {
		return nil, err
	}
	return shift, nil
}
