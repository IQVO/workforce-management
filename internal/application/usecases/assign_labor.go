package usecases

import (
	"context"
	"errors"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// AssignLabor puts an associate on a path for a new interval. It validates
// the associate holds the path's required certification and is not on
// break, then ends any prior ACTIVE assignment for that associate before
// starting the new one — the double-booking invariant is enforced by
// construction (see assignment.LaborAssignment), never by rejecting the
// second call.
//
// Path -> required certification is a naming convention in this context: a
// path's required certification is the Certification with the same name as
// its PathId (e.g. path "pack" requires certification "pack"). This context
// owns no separate path-requirements concept, so the convention avoids
// inventing one.
type AssignLabor struct {
	Associates       ports.AssociateRepo
	Assignments      ports.AssignmentRepo
	Events           ports.EventPublisher
	Clock            ports.Clock
	MaxHoursPerShift float64
	// Metrics records every attempt on the workforce.labor_assignments
	// counter (ADR-0015 Tier 2). Optional: nil means "not instrumented".
	Metrics ports.LaborMetrics
	// UnitOfWork brackets every Save and the Publish atomically (ADR 0016).
	// Optional: nil means "no transactional backing" and the calls run back
	// to back, which is the in-memory / log-publisher configuration.
	UnitOfWork ports.UnitOfWork
}

// Execute assigns associateId to pathId.
//
// Every attempt — accepted or rejected — is counted on the
// workforce.labor_assignments metric. That is observation only: it does not
// change what this use case decides or returns.
func (uc *AssignLabor) Execute(ctx context.Context, associateId shared.AssociateId, pathId shared.PathId) (result *assignment.LaborAssignment, err error) {
	defer func() { recordLaborAssignment(ctx, uc.Metrics, pathId, err) }()

	shift, err := uc.Associates.FindByID(ctx, associateId)
	if err != nil {
		return nil, err
	}
	if err := shift.CanBeAssigned(); err != nil {
		return nil, err
	}

	requiredCert := shared.Certification(pathId)
	hasCert := shift.HasCertification(requiredCert)

	la, err := uc.Assignments.FindByAssociateID(ctx, associateId)
	if err != nil {
		if !errors.Is(err, ports.ErrNotFound) {
			return nil, err
		}
		la = assignment.NewLaborAssignment(associateId)
	}

	now := uc.Clock.Now()
	priorHistoryLen := len(la.History())
	if err := la.Assign(pathId, hasCert, now); err != nil {
		return nil, err
	}

	shiftChanged := false
	if newHistory := la.History(); len(newHistory) > priorHistoryLen {
		closed := newHistory[len(newHistory)-1]
		if err := shift.LogHours(closed.Hours(now), uc.MaxHoursPerShift); err != nil {
			return nil, err
		}
		shiftChanged = true
	}

	err = atomically(ctx, uc.UnitOfWork, func(ctx context.Context) error {
		if shiftChanged {
			if err := uc.Associates.Save(ctx, shift); err != nil {
				return err
			}
		}
		if err := uc.Assignments.Save(ctx, la); err != nil {
			return err
		}
		return uc.Events.Publish(ctx, la.PullEvents()...)
	})
	if err != nil {
		return nil, err
	}
	return la, nil
}
