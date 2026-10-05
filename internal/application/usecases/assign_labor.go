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
// path's required certification is, by default, the Certification with the
// same name as the PathId (e.g. path "pack" requires certification
// "pack"). When a Catalogue is wired, that name is the RESOLVED path
// family's canonical prefix rather than the raw caller-supplied id: the
// catalogue (ADR-0013) recognizes "PICK", "pick" and "pick-zone-a" as one
// family, so all three require certification "pick" — an exact raw-id
// match here would reject a certified associate over a case variant or
// zone suffix (the 2026-10 ADR-conformance fix; see ADR-0009's amendment).
type AssignLabor struct {
	Associates       ports.AssociateRepo
	Assignments      ports.AssignmentRepo
	Events           ports.EventPublisher
	Clock            ports.Clock
	MaxHoursPerShift float64
	// Catalogue normalises the required-certification name through the
	// path's canonical family (ADR-0013). Optional: nil keeps the raw
	// pathId-as-certification convention, which is the in-memory test
	// configuration's behaviour.
	Catalogue ports.PathCatalogue
	// Metrics records every attempt on the workforce.labor_assignments
	// counter (ADR-0015 Tier 2). Optional: nil means "not instrumented".
	Metrics ports.LaborMetrics
	// UnitOfWork brackets every Save and the Publish atomically (ADR 0016).
	// Optional: nil means "no transactional backing" and the calls run back
	// to back, which is the in-memory / log-publisher configuration.
	UnitOfWork ports.UnitOfWork
}

// requiredCertification resolves the certification a path requires. A
// successful catalogue lookup yields the family's canonical prefix (the
// lower-case form certifications are documented in: "pack", "hazmat",
// "pick"); anything else falls back to the raw pathId, preserving the
// historical convention for catalogue-less configurations.
func requiredCertification(catalogue ports.PathCatalogue, pathId shared.PathId) shared.Certification {
	if catalogue != nil {
		if def, err := catalogue.Lookup(string(pathId)); err == nil && def.MatchPrefix != "" {
			return shared.Certification(def.MatchPrefix)
		}
	}
	return shared.Certification(pathId)
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

	requiredCert := requiredCertification(uc.Catalogue, pathId)
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
