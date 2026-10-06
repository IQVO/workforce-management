package usecases

import (
	"context"
	"errors"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// Rejection-reason slugs for ports.LaborMetrics.AssignmentRejected. Every
// value is drawn from a closed set so the metric stays low-cardinality
// (ADR-0015 Tier 2); the mapping error -> slug lives HERE because only the
// application layer knows which domain sentinel each failure is.
const (
	ReasonUncertified       = "uncertified"
	ReasonOnBreak           = "on_break"
	ReasonShiftEnded        = "shift_ended"
	ReasonMaxHoursExceeded  = "max_hours_exceeded"
	ReasonAssociateNotFound = "associate_not_found"
	ReasonInternalError     = "internal_error"
)

// recordLaborAssignment records one AssignLabor attempt on the use case's
// ports.LaborMetrics port (ADR-0015 Tier 2: the instrument itself lives in
// the outbound telemetry adapter; this layer only maps the outcome onto the
// port). A nil metrics port means "not instrumented" and is a no-op. A nil
// err is an accepted assignment; anything else is a rejection tagged with a
// closed-set reason so the "why are we failing to staff this path" question
// is answerable from the metric alone.
func recordLaborAssignment(ctx context.Context, metrics ports.LaborMetrics, pathId shared.PathId, err error) {
	if metrics == nil {
		return
	}
	if err == nil {
		metrics.AssignmentAccepted(ctx, pathId)
		return
	}
	metrics.AssignmentRejected(ctx, pathId, RejectionReason(err))
}

// RejectionReason maps a rejection to a stable low-cardinality slug.
//
// Note there is no "double_booked" reason: this context resolves a second
// active assignment by ending the prior one and raising LaborReassigned (see
// LaborAssignment.Assign), so double-booking is prevented by construction
// rather than rejected. The gates that genuinely reject are certification,
// break state, shift state and the max-hours limit.
func RejectionReason(err error) string {
	switch {
	case errors.Is(err, assignment.ErrCertificationRequired):
		return ReasonUncertified
	case errors.Is(err, associate.ErrOnBreak):
		return ReasonOnBreak
	case errors.Is(err, associate.ErrShiftEnded):
		return ReasonShiftEnded
	case errors.Is(err, associate.ErrMaxHoursExceeded):
		return ReasonMaxHoursExceeded
	case errors.Is(err, ports.ErrNotFound):
		return ReasonAssociateNotFound
	default:
		return ReasonInternalError
	}
}
