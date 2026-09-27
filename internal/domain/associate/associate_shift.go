// Package associate holds the AssociateShift aggregate: who is on shift,
// their certifications, and their break state.
package associate

import (
	"errors"
	"time"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// ErrAlreadyOnBreak is returned when StartBreak is called while already on a
// break.
var ErrAlreadyOnBreak = errors.New("associate is already on break")

// ErrNotOnBreak is returned when EndBreak is called while not on a break.
var ErrNotOnBreak = errors.New("associate is not on break")

// ErrOnBreak is returned when an assignment is attempted while the associate
// is on a logged break.
var ErrOnBreak = errors.New("associate is on break and cannot be assigned")

// ErrMaxHoursExceeded is returned when logging hours would exceed the
// configured max-hours-per-shift limit.
var ErrMaxHoursExceeded = errors.New("logging these hours would exceed the max hours per shift")

// ErrShiftEnded is returned when an operation is attempted on a shift that
// has already ended.
var ErrShiftEnded = errors.New("associate shift has already ended")

// AssociateShift is the aggregate root for a single associate's roster entry
// for a shift: their certifications, break state, and logged hours.
type AssociateShift struct {
	associateId    shared.AssociateId
	certifications map[shared.Certification]struct{}
	onBreak        bool
	hoursLogged    float64
	ended          bool

	// version is inert optimistic-concurrency infrastructure metadata
	// (see ADR 0021) -- the domain layer carries it but never reasons
	// about it in business logic, exactly like associateId's identity
	// role. A fresh aggregate starts at 1; AssociateRepo increments it
	// on every successful Save.
	version int

	events []shared.DomainEvent
}

// NewAssociateShift starts a shift for an associate with an initial set of
// certifications, raising AssociateShiftStarted.
func NewAssociateShift(associateId shared.AssociateId, certifications []shared.Certification, at time.Time) *AssociateShift {
	certs := make(map[shared.Certification]struct{}, len(certifications))
	for _, c := range certifications {
		certs[c] = struct{}{}
	}
	a := &AssociateShift{
		associateId:    associateId,
		certifications: certs,
		version:        1,
	}
	a.record(shared.NewAssociateShiftStarted(at, associateId, certifications))
	return a
}

// Rehydrate reconstructs an AssociateShift from persisted state without
// raising events. Adapters use this to load an aggregate from storage.
// version is the value the aggregate was loaded at (see ADR 0021,
// optimistic concurrency) -- adapters pass through whatever they read.
func Rehydrate(associateId shared.AssociateId, certifications []shared.Certification, onBreak bool, hoursLogged float64, ended bool, version int) *AssociateShift {
	certs := make(map[shared.Certification]struct{}, len(certifications))
	for _, c := range certifications {
		certs[c] = struct{}{}
	}
	return &AssociateShift{
		associateId:    associateId,
		certifications: certs,
		onBreak:        onBreak,
		hoursLogged:    hoursLogged,
		ended:          ended,
		version:        version,
	}
}

// Certifications returns the associate's current certifications.
func (a *AssociateShift) Certifications() []shared.Certification {
	certs := make([]shared.Certification, 0, len(a.certifications))
	for c := range a.certifications {
		certs = append(certs, c)
	}
	return certs
}

// AssociateId returns the associate's identity.
func (a *AssociateShift) AssociateId() shared.AssociateId { return a.associateId }

// Version returns the optimistic-concurrency version this aggregate was
// loaded at (see ADR 0021). Infrastructure metadata only -- domain logic
// never branches on it.
func (a *AssociateShift) Version() int { return a.version }

// IsOnBreak reports whether the associate is currently on a logged break.
func (a *AssociateShift) IsOnBreak() bool { return a.onBreak }

// HoursLogged returns the associate's accumulated hours for this shift.
func (a *AssociateShift) HoursLogged() float64 { return a.hoursLogged }

// Ended reports whether the shift has closed.
func (a *AssociateShift) Ended() bool { return a.ended }

// HasCertification reports whether the associate holds the given
// certification.
func (a *AssociateShift) HasCertification(c shared.Certification) bool {
	_, ok := a.certifications[c]
	return ok
}

// Certify adds a certification to the associate, raising AssociateCertified.
func (a *AssociateShift) Certify(c shared.Certification, at time.Time) error {
	if a.ended {
		return ErrShiftEnded
	}
	a.certifications[c] = struct{}{}
	a.record(shared.NewAssociateCertified(at, a.associateId, c))
	return nil
}

// StartBreak begins a logged break. It is rejected if the associate is
// already on break or the shift has ended.
func (a *AssociateShift) StartBreak(at time.Time) error {
	if a.ended {
		return ErrShiftEnded
	}
	if a.onBreak {
		return ErrAlreadyOnBreak
	}
	a.onBreak = true
	a.record(shared.NewAssociateBreakStarted(at, a.associateId))
	return nil
}

// EndBreak ends a logged break. It is rejected if the associate is not on
// break.
func (a *AssociateShift) EndBreak(at time.Time) error {
	if a.ended {
		return ErrShiftEnded
	}
	if !a.onBreak {
		return ErrNotOnBreak
	}
	a.onBreak = false
	a.record(shared.NewAssociateBreakEnded(at, a.associateId))
	return nil
}

// CanBeAssigned reports whether the associate is eligible to receive a new
// labor assignment: not on break and the shift has not ended.
func (a *AssociateShift) CanBeAssigned() error {
	if a.ended {
		return ErrShiftEnded
	}
	if a.onBreak {
		return ErrOnBreak
	}
	return nil
}

// LogHours adds hours to the associate's total for this shift. It is
// rejected if the addition would exceed maxHoursPerShift.
func (a *AssociateShift) LogHours(hours float64, maxHoursPerShift float64) error {
	if a.ended {
		return ErrShiftEnded
	}
	if a.hoursLogged+hours > maxHoursPerShift {
		return ErrMaxHoursExceeded
	}
	a.hoursLogged += hours
	return nil
}

// EndShift closes the shift, raising AssociateShiftEnded. Ending an
// already-ended shift is a no-op that raises no event.
func (a *AssociateShift) EndShift(at time.Time) {
	if a.ended {
		return
	}
	a.ended = true
	a.record(shared.NewAssociateShiftEnded(at, a.associateId))
}

func (a *AssociateShift) record(e shared.DomainEvent) {
	a.events = append(a.events, e)
}

// PullEvents returns and clears the events raised since the last call.
func (a *AssociateShift) PullEvents() []shared.DomainEvent {
	events := a.events
	a.events = nil
	return events
}
