// Package shiftplan holds the ShiftPlan aggregate: the committed split of
// headcount across paths for one shift, plus the pure headcount-proposal
// computation used before a human commits it.
package shiftplan

import (
	"errors"
	"math"
	"time"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// ErrNoPathPlans is returned when a ShiftPlan is committed with no lines.
var ErrNoPathPlans = errors.New("shift plan must have at least one path plan line")

// ErrPlannedHeadsExceedInstalled is returned when a PathPlan's plannedHeads
// exceeds the path's installed station count.
var ErrPlannedHeadsExceedInstalled = errors.New("planned heads exceed installed stations for path")

// ErrPlannedHoursExceedCapacity is returned when a PathPlan's plannedHours
// exceeds what its planned heads can work within the shift's max hours.
var ErrPlannedHoursExceedCapacity = errors.New("planned hours exceed capacity for planned heads within max hours per shift")

// ErrMissingInstalledStations is returned when a PathPlan line references a
// path with no known installed-station count.
var ErrMissingInstalledStations = errors.New("missing installed station count for path")

// ErrExceedsInstalledCapacity is returned when a PathPlan's plannedHeads
// exceeds the path's LIVE installed capacity, as sourced from
// fulfillment-execution's real Station registry at commit time. This is
// deliberately a SEPARATE, independent check from
// ErrPlannedHeadsExceedInstalled: the latter validates against a
// caller-supplied installedStations number (which this context has no
// way to verify on its own), while this one validates against a live
// fetch of physical reality. See ADR-0014.
var ErrExceedsInstalledCapacity = errors.New("planned heads exceed the live installed capacity reported by fulfillment-execution")

// PathPlan is one line of a ShiftPlan: the committed headcount, rate, and
// hours for a single path.
type PathPlan struct {
	PathId       shared.PathId
	PlannedHeads int
	PlannedRate  float64
	PlannedHours float64
}

// ShiftPlan is the aggregate root for the committed headcount split across
// paths for one building's shift.
type ShiftPlan struct {
	buildingId string
	shiftId    string
	lines      []PathPlan

	events []shared.DomainEvent
}

// ProposedHeads is the pure computation behind ProposePathPlan: heads needed
// to cover a path's charge at the given planned rate, rounded up. It commits
// nothing and has no aggregate identity.
func ProposedHeads(charge float64, plannedRate float64) int {
	if plannedRate <= 0 {
		return 0
	}
	return int(math.Ceil(charge / plannedRate))
}

// CommitShiftPlan validates and constructs a committed ShiftPlan, raising
// ShiftPlanCommitted. installedStations must contain an entry for every path
// referenced by lines (a caller-supplied, structural check).
// installedCapacity is the LIVE, fulfillment-execution-sourced ceiling for
// each path (Feature C) -- a SEPARATE, independent check: a missing entry
// is treated as a live capacity of 0, never skipped, so a caller cannot
// bypass this check simply by omitting a path. maxHoursPerShift bounds
// each line's plannedHours to what its planned heads can work in one
// shift.
func CommitShiftPlan(buildingId, shiftId string, lines []PathPlan, installedStations map[shared.PathId]int, installedCapacity map[shared.PathId]int, maxHoursPerShift float64, at time.Time) (*ShiftPlan, error) {
	if len(lines) == 0 {
		return nil, ErrNoPathPlans
	}
	for _, line := range lines {
		installed, ok := installedStations[line.PathId]
		if !ok {
			return nil, ErrMissingInstalledStations
		}
		if line.PlannedHeads > installed {
			return nil, ErrPlannedHeadsExceedInstalled
		}
		// installedCapacity[line.PathId] defaults to the zero value (0)
		// for a path with no entry -- deliberately a ceiling of 0, not
		// "skip this check" -- so a missing live-capacity fetch can
		// never silently bypass Feature C's invariant.
		if line.PlannedHeads > installedCapacity[line.PathId] {
			return nil, ErrExceedsInstalledCapacity
		}
		if line.PlannedHours > float64(line.PlannedHeads)*maxHoursPerShift {
			return nil, ErrPlannedHoursExceedCapacity
		}
	}
	sp := &ShiftPlan{
		buildingId: buildingId,
		shiftId:    shiftId,
		lines:      append([]PathPlan(nil), lines...),
	}
	sp.record(shared.NewShiftPlanCommitted(at, buildingId, shiftId))
	return sp, nil
}

// Rehydrate reconstructs a ShiftPlan from persisted state without raising
// events.
func Rehydrate(buildingId, shiftId string, lines []PathPlan) *ShiftPlan {
	return &ShiftPlan{
		buildingId: buildingId,
		shiftId:    shiftId,
		lines:      append([]PathPlan(nil), lines...),
	}
}

// SiteCode returns the site this plan was committed for -- the canonical name
// of the plan key (ADR 0035).
func (s *ShiftPlan) SiteCode() string { return s.buildingId }

// BuildingId returns the same value as SiteCode. It is the deprecated alias
// (written without the godoc "Deprecated:" marker on purpose: the persistence
// and event adapters still read the stored building_id name, and staticcheck
// would flag every such use); buildingId is removed only by a later, announced
// breaking ADR.
func (s *ShiftPlan) BuildingId() string { return s.buildingId }

// ShiftId returns the shift this plan was committed for.
func (s *ShiftPlan) ShiftId() string { return s.shiftId }

// Lines returns the committed PathPlan lines.
func (s *ShiftPlan) Lines() []PathPlan { return append([]PathPlan(nil), s.lines...) }

// PlannedHeadsFor returns the committed heads for a path, or 0 if the path
// has no line in this plan.
func (s *ShiftPlan) PlannedHeadsFor(pathId shared.PathId) int {
	for _, line := range s.lines {
		if line.PathId == pathId {
			return line.PlannedHeads
		}
	}
	return 0
}

func (s *ShiftPlan) record(e shared.DomainEvent) {
	s.events = append(s.events, e)
}

// PullEvents returns and clears the events raised since the last call.
func (s *ShiftPlan) PullEvents() []shared.DomainEvent {
	events := s.events
	s.events = nil
	return events
}
