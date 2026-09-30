// Package assignment holds the LaborAssignment aggregate: one associate, one
// path, an interval. The aggregate root is keyed by associate so that
// "exactly one ACTIVE assignment per associate" is a structural invariant —
// there is only ever one active-interval field to hold it in.
package assignment

import (
	"errors"
	"time"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// ErrCertificationRequired is returned when an associate lacks the
// certification a path requires.
var ErrCertificationRequired = errors.New("associate lacks the certification required for this path")

// Interval is a span of time an associate spent assigned to a path.
// End is nil while the interval is active.
type Interval struct {
	PathId shared.PathId
	Start  time.Time
	End    *time.Time
}

// Hours returns the interval's duration in hours as of `at` if still active,
// or its recorded duration if closed.
func (iv Interval) Hours(at time.Time) float64 {
	end := at
	if iv.End != nil {
		end = *iv.End
	}
	return end.Sub(iv.Start).Hours()
}

// LaborAssignment is the aggregate root tracking one associate's current
// path assignment and assignment history.
type LaborAssignment struct {
	associateId shared.AssociateId
	active      *Interval
	history     []Interval

	// version is inert optimistic-concurrency infrastructure metadata
	// (see ADR 0021) -- carried but never reasoned about by domain
	// logic. A fresh aggregate starts at 1; AssignmentRepo increments
	// it on every successful Save.
	version int

	events []shared.DomainEvent
}

// NewLaborAssignment starts an empty assignment record for an associate, with
// no active assignment yet.
func NewLaborAssignment(associateId shared.AssociateId) *LaborAssignment {
	return &LaborAssignment{associateId: associateId, version: 1}
}

// Rehydrate reconstructs a LaborAssignment from persisted state without
// raising events. version is the value the aggregate was loaded at (see
// ADR 0021, optimistic concurrency) -- adapters pass through whatever they
// read.
func Rehydrate(associateId shared.AssociateId, active *Interval, history []Interval, version int) *LaborAssignment {
	return &LaborAssignment{
		associateId: associateId,
		active:      active,
		history:     append([]Interval(nil), history...),
		version:     version,
	}
}

// AssociateId returns the associate this assignment record belongs to.
func (l *LaborAssignment) AssociateId() shared.AssociateId { return l.associateId }

// Version returns the optimistic-concurrency version this aggregate was
// loaded at (see ADR 0021). Infrastructure metadata only -- domain logic
// never branches on it.
func (l *LaborAssignment) Version() int { return l.version }

// IsActive reports whether the associate currently has an active assignment.
func (l *LaborAssignment) IsActive() bool { return l.active != nil }

// ActivePathId returns the path of the current active assignment, if any.
func (l *LaborAssignment) ActivePathId() (shared.PathId, bool) {
	if l.active == nil {
		return "", false
	}
	return l.active.PathId, true
}

// ActiveInterval returns a copy of the current active interval, if any.
// Adapters use this to persist the active assignment's start time.
func (l *LaborAssignment) ActiveInterval() (Interval, bool) {
	if l.active == nil {
		return Interval{}, false
	}
	return *l.active, true
}

// History returns closed intervals.
func (l *LaborAssignment) History() []Interval { return append([]Interval(nil), l.history...) }

// Assign puts the associate on pathId for a new interval starting at `at`.
// It requires hasCertification to be true, or it is rejected. If an
// assignment is already active, it is closed first and LaborReassigned is
// raised; otherwise LaborAssigned is raised.
func (l *LaborAssignment) Assign(pathId shared.PathId, hasCertification bool, at time.Time) error {
	if !hasCertification {
		return ErrCertificationRequired
	}
	if l.active != nil {
		fromPath := l.active.PathId
		l.closeActive(at)
		l.active = &Interval{PathId: pathId, Start: at}
		l.record(shared.NewLaborReassigned(at, l.associateId, fromPath, pathId))
		return nil
	}
	l.active = &Interval{PathId: pathId, Start: at}
	l.record(shared.NewLaborAssigned(at, l.associateId, pathId))
	return nil
}

// EndActive closes the current active assignment, if any, moving it to
// history. It returns the closed interval and whether one was active.
func (l *LaborAssignment) EndActive(at time.Time) (Interval, bool) {
	if l.active == nil {
		return Interval{}, false
	}
	closed := *l.active
	l.closeActive(at)
	return closed, true
}

func (l *LaborAssignment) closeActive(at time.Time) {
	if l.active == nil {
		return
	}
	end := at
	l.active.End = &end
	l.history = append(l.history, *l.active)
	l.active = nil
}

func (l *LaborAssignment) record(e shared.DomainEvent) {
	l.events = append(l.events, e)
}

// PullEvents returns and clears the events raised since the last call.
func (l *LaborAssignment) PullEvents() []shared.DomainEvent {
	events := l.events
	l.events = nil
	return events
}
