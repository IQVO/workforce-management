package usecases

import (
	"context"
	"errors"
	"testing"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

func TestRejectionReasonMapsEveryGate(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "uncertified", err: assignment.ErrCertificationRequired, want: ReasonUncertified},
		{name: "on break", err: associate.ErrOnBreak, want: ReasonOnBreak},
		{name: "shift ended", err: associate.ErrShiftEnded, want: ReasonShiftEnded},
		{name: "max hours", err: associate.ErrMaxHoursExceeded, want: ReasonMaxHoursExceeded},
		{name: "unknown associate", err: ports.ErrNotFound, want: ReasonAssociateNotFound},
		{name: "wrapped sentinel still maps", err: errors.Join(errors.New("save failed"), associate.ErrOnBreak), want: ReasonOnBreak},
		{name: "anything else", err: errors.New("boom"), want: ReasonInternalError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RejectionReason(tc.err); got != tc.want {
				t.Errorf("RejectionReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// recordingLaborMetrics is a fake ports.LaborMetrics capturing every call,
// so the port-level contract (accepted has no reason; rejected carries the
// mapped slug) is testable without any OTel SDK in this package.
type recordingLaborMetrics struct {
	accepted []shared.PathId
	rejected []rejectedCall
}

type rejectedCall struct {
	pathId shared.PathId
	reason string
}

func (m *recordingLaborMetrics) AssignmentAccepted(_ context.Context, pathId shared.PathId) {
	m.accepted = append(m.accepted, pathId)
}

func (m *recordingLaborMetrics) AssignmentRejected(_ context.Context, pathId shared.PathId, reason string) {
	m.rejected = append(m.rejected, rejectedCall{pathId: pathId, reason: reason})
}

func TestRecordLaborAssignmentRoutesOutcomesOntoThePort(t *testing.T) {
	m := &recordingLaborMetrics{}
	ctx := context.Background()

	recordLaborAssignment(ctx, m, "pick-zone-a", nil)
	recordLaborAssignment(ctx, m, "pick-zone-a", nil)
	recordLaborAssignment(ctx, m, "pack", assignment.ErrCertificationRequired)

	if len(m.accepted) != 2 || m.accepted[0] != "pick-zone-a" || m.accepted[1] != "pick-zone-a" {
		t.Errorf("accepted = %v, want two pick-zone-a", m.accepted)
	}
	if len(m.rejected) != 1 || m.rejected[0].pathId != "pack" || m.rejected[0].reason != ReasonUncertified {
		t.Errorf("rejected = %v, want pack/uncertified", m.rejected)
	}
}

// A nil metrics port means "not instrumented" and must be a silent no-op
// for BOTH outcomes, never a panic.
func TestRecordLaborAssignmentNilPortIsNoOp(t *testing.T) {
	recordLaborAssignment(context.Background(), nil, "nil-port-guard", nil)
	recordLaborAssignment(context.Background(), nil, "nil-port-guard", assignment.ErrCertificationRequired)
}
