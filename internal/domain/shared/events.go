package shared

import "time"

// DomainEvent is a past-tense fact raised by an aggregate. Aggregates never
// read the wall clock themselves; callers (use cases, via the Clock port)
// supply the occurrence time so domain logic stays deterministic and testable.
type DomainEvent interface {
	EventName() string
	OccurredAt() time.Time
}

type baseEvent struct {
	occurredAt time.Time
}

// OccurredAt returns when the event happened.
func (b baseEvent) OccurredAt() time.Time { return b.occurredAt }

// ShiftPlanProposed is raised when heads for a path are computed from charge
// and planned rate, before any human commits them.
type ShiftPlanProposed struct {
	baseEvent
	BuildingId   string
	PathId       PathId
	PlannedHeads int
	PlannedRate  float64
}

// EventName implements DomainEvent.
func (ShiftPlanProposed) EventName() string { return "ShiftPlanProposed" }

// SiteCode is the canonical name of the plan key carried as BuildingId
// (ADR 0035): the same value, so the published payload's `site_code` and its
// deprecated `building_id` can never differ.
func (e ShiftPlanProposed) SiteCode() string { return e.BuildingId }

// NewShiftPlanProposed constructs a ShiftPlanProposed event.
func NewShiftPlanProposed(at time.Time, buildingId string, pathId PathId, plannedHeads int, plannedRate float64) ShiftPlanProposed {
	return ShiftPlanProposed{baseEvent{at}, buildingId, pathId, plannedHeads, plannedRate}
}

// ShiftPlanCommitted is raised when a human commits a headcount split across
// paths for a building's shift.
type ShiftPlanCommitted struct {
	baseEvent
	BuildingId string
	ShiftId    string
}

// EventName implements DomainEvent.
func (ShiftPlanCommitted) EventName() string { return "ShiftPlanCommitted" }

// SiteCode is the canonical name of the plan key carried as BuildingId
// (ADR 0035): the same value.
func (e ShiftPlanCommitted) SiteCode() string { return e.BuildingId }

// NewShiftPlanCommitted constructs a ShiftPlanCommitted event.
func NewShiftPlanCommitted(at time.Time, buildingId, shiftId string) ShiftPlanCommitted {
	return ShiftPlanCommitted{baseEvent{at}, buildingId, shiftId}
}

// AssociateShiftStarted is raised when an associate's shift roster entry is
// created.
type AssociateShiftStarted struct {
	baseEvent
	AssociateId    AssociateId
	Certifications []Certification
}

// EventName implements DomainEvent.
func (AssociateShiftStarted) EventName() string { return "AssociateShiftStarted" }

// NewAssociateShiftStarted constructs an AssociateShiftStarted event.
func NewAssociateShiftStarted(at time.Time, associateId AssociateId, certifications []Certification) AssociateShiftStarted {
	return AssociateShiftStarted{baseEvent{at}, associateId, certifications}
}

// AssociateCertified is raised when a certification is added to an associate.
type AssociateCertified struct {
	baseEvent
	AssociateId   AssociateId
	Certification Certification
}

// EventName implements DomainEvent.
func (AssociateCertified) EventName() string { return "AssociateCertified" }

// NewAssociateCertified constructs an AssociateCertified event.
func NewAssociateCertified(at time.Time, associateId AssociateId, certification Certification) AssociateCertified {
	return AssociateCertified{baseEvent{at}, associateId, certification}
}

// AssociateBreakStarted is raised when an associate begins a logged break.
type AssociateBreakStarted struct {
	baseEvent
	AssociateId AssociateId
}

// EventName implements DomainEvent.
func (AssociateBreakStarted) EventName() string { return "AssociateBreakStarted" }

// NewAssociateBreakStarted constructs an AssociateBreakStarted event.
func NewAssociateBreakStarted(at time.Time, associateId AssociateId) AssociateBreakStarted {
	return AssociateBreakStarted{baseEvent{at}, associateId}
}

// AssociateBreakEnded is raised when an associate ends a logged break.
type AssociateBreakEnded struct {
	baseEvent
	AssociateId AssociateId
}

// EventName implements DomainEvent.
func (AssociateBreakEnded) EventName() string { return "AssociateBreakEnded" }

// NewAssociateBreakEnded constructs an AssociateBreakEnded event.
func NewAssociateBreakEnded(at time.Time, associateId AssociateId) AssociateBreakEnded {
	return AssociateBreakEnded{baseEvent{at}, associateId}
}

// LaborAssigned is raised when an associate is assigned to a path for an
// interval.
type LaborAssigned struct {
	baseEvent
	AssociateId AssociateId
	PathId      PathId
}

// EventName implements DomainEvent.
func (LaborAssigned) EventName() string { return "LaborAssigned" }

// NewLaborAssigned constructs a LaborAssigned event.
func NewLaborAssigned(at time.Time, associateId AssociateId, pathId PathId) LaborAssigned {
	return LaborAssigned{baseEvent{at}, associateId, pathId}
}

// LaborReassigned is raised when an associate's prior active assignment is
// ended in favor of a new path assignment.
type LaborReassigned struct {
	baseEvent
	AssociateId AssociateId
	FromPathId  PathId
	ToPathId    PathId
}

// EventName implements DomainEvent.
func (LaborReassigned) EventName() string { return "LaborReassigned" }

// NewLaborReassigned constructs a LaborReassigned event.
func NewLaborReassigned(at time.Time, associateId AssociateId, fromPathId, toPathId PathId) LaborReassigned {
	return LaborReassigned{baseEvent{at}, associateId, fromPathId, toPathId}
}

// PathUnderstaffed is raised when a path's active assignments fall short of
// its planned heads. It is a flag, not a decision — this context never moves
// anyone in response.
type PathUnderstaffed struct {
	baseEvent
	PathId       PathId
	PlannedHeads int
	ActiveHeads  int
	// SiteCode is the canonical site the gap was computed for. Empty means
	// the gap was computed fleet-wide (unscoped); consumers must treat the
	// absence of a site as "unscoped", never as a particular site (ADR 0034).
	SiteCode SiteCode
}

// EventName implements DomainEvent.
func (PathUnderstaffed) EventName() string { return "PathUnderstaffed" }

// NewPathUnderstaffed constructs an unscoped (fleet-wide) PathUnderstaffed event.
func NewPathUnderstaffed(at time.Time, pathId PathId, plannedHeads, activeHeads int) PathUnderstaffed {
	return NewPathUnderstaffedAtSite(at, pathId, plannedHeads, activeHeads, "")
}

// NewPathUnderstaffedAtSite constructs a PathUnderstaffed event for a gap
// computed against one site's staff only. An empty siteCode is the unscoped
// event, identical to NewPathUnderstaffed.
func NewPathUnderstaffedAtSite(at time.Time, pathId PathId, plannedHeads, activeHeads int, siteCode SiteCode) PathUnderstaffed {
	return PathUnderstaffed{baseEvent{at}, pathId, plannedHeads, activeHeads, siteCode}
}

// AssociateShiftEnded is raised when an associate's shift closes, ending all
// active assignments.
type AssociateShiftEnded struct {
	baseEvent
	AssociateId AssociateId
}

// EventName implements DomainEvent.
func (AssociateShiftEnded) EventName() string { return "AssociateShiftEnded" }

// NewAssociateShiftEnded constructs an AssociateShiftEnded event.
func NewAssociateShiftEnded(at time.Time, associateId AssociateId) AssociateShiftEnded {
	return AssociateShiftEnded{baseEvent{at}, associateId}
}
