// Package ports declares the outbound interfaces the application layer
// depends on: repositories for each aggregate, an event publisher, and a
// clock. Adapters implement these; the application layer never imports an
// adapter package.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// AssociateRepo persists and loads AssociateShift aggregates. Save is
// version-guarded (see ADR 0021, optimistic concurrency): it returns
// ports.ErrConcurrentModification if the row's current version no longer
// matches the version the caller's aggregate was loaded at.
type AssociateRepo interface {
	Save(ctx context.Context, a *associate.AssociateShift) error
	FindByID(ctx context.Context, id shared.AssociateId) (*associate.AssociateShift, error)
}

// ShiftPlanRepo persists and loads ShiftPlan aggregates. A plan is keyed by
// buildingId + shiftId.
type ShiftPlanRepo interface {
	Save(ctx context.Context, sp *shiftplan.ShiftPlan) error
	FindByBuildingAndShift(ctx context.Context, buildingId, shiftId string) (*shiftplan.ShiftPlan, error)
}

// AssignmentRepo persists and loads LaborAssignment aggregates, one per
// associate, and answers staffing-gap queries. Save is version-guarded
// (see ADR 0021, optimistic concurrency): it returns
// ports.ErrConcurrentModification if the row's current version no longer
// matches the version the caller's aggregate was loaded at.
type AssignmentRepo interface {
	Save(ctx context.Context, la *assignment.LaborAssignment) error
	FindByAssociateID(ctx context.Context, id shared.AssociateId) (*assignment.LaborAssignment, error)
	CountActiveByPath(ctx context.Context, pathId shared.PathId) (int, error)
}

// MeasuredRateClient queries labor-performance for a real, measured mean
// task duration for a path, so ProposePathPlan can propose headcount
// against reality rather than requiring a caller-supplied plannedRate
// every time. See ADR-000X in this repo's docs for the full "why a new
// outbound Supplier, why fail-open" rationale.
type MeasuredRateClient interface {
	// MeanActualSeconds returns the real measured mean duration (seconds)
	// labor-performance has recorded for pathId, or
	// ErrMeasuredRateUnavailable on ANY failure to produce one --
	// unreachable service, malformed response, or genuinely no data yet.
	// ProposePathPlan treats every ErrMeasuredRateUnavailable case
	// identically: fall back to the caller-supplied plannedRate. It never
	// needs (and this interface deliberately never exposes) a way to tell
	// those failure modes apart.
	MeanActualSeconds(ctx context.Context, pathId shared.PathId) (float64, error)
}

// ErrMeasuredRateUnavailable is returned by a MeasuredRateClient
// implementation for every failure mode a caller cannot usefully act on
// differently: an unreachable labor-performance, a malformed response, or
// a real 200 response reporting no measurable data yet for this path.
// ProposePathPlan's single error-handling branch depends on this being
// the ONLY error a MeasuredRateClient ever returns.

// IdleShareClient reports an observed idle share for a path's task type --
// the fraction of that TaskType's clocked time (task-doing time plus
// between-task idle waiting time) that was idle, fed back from
// labor-performance's idleness signal via this service's
// laborperformancecache.Consumer (ADR addendum: idle-share staffing
// signal). Unlike MeasuredRateClient, only the kafka-cache
// LABOR_PERFORMANCE_MODE implements this port today -- the http and
// permissive modes have no per-message idle_seconds_before stream to
// observe, so a composition root running either of those leaves the field
// this is wired through nil, and both GetStaffingGap and ProposePathPlan
// already treat a nil IdleShareClient as "no signal, no trim/no
// surfacing" -- the same fail-open discipline every other *_MODE in this
// fleet defaults to.
type IdleShareClient interface {
	// IdleSharePct returns the observed idle share (a fraction in [0, 1],
	// NOT a 0-100 percentage) for pathId's task type, or
	// ErrIdleShareUnavailable on ANY failure to produce one -- no TaskType
	// mapping for pathId, or no TaskPerformanceRecorded message carrying a
	// non-nil idle_seconds_before has been observed yet for that TaskType.
	// A nil idle_seconds_before means "not observed" (first-ever
	// completion, a negative/zero gap from Kafka reordering, or an empty
	// AssociateId e.g. a robot station) -- it is NEVER coerced into a 0
	// share, so a TaskType with data for its mean but none for idle share
	// yet is reported unavailable, not a false 0.
	IdleSharePct(ctx context.Context, pathId shared.PathId) (float64, error)
}

// ErrIdleShareUnavailable is returned by an IdleShareClient implementation
// for every failure mode a caller cannot usefully act on differently:
// no TaskType mapping for this path, or genuinely no idle-share data
// observed yet. GetStaffingGap and ProposePathPlan both fail open on it --
// surface nothing / trim nothing -- exactly mirroring
// ErrMeasuredRateUnavailable's contract.
var ErrIdleShareUnavailable = errors.New("idle share unavailable")

// InstalledCapacityClient queries fulfillment-execution for the real,
// live count of registered stations that hold a path's capability, so
// CommitShiftPlan can enforce plannedHeads against physical reality
// instead of trusting a caller-supplied installedStations count alone.
// See ADR-0014 in this repo's docs for the full "why a new outbound
// Supplier, why fail-LOUD (unlike MeasuredRateClient's fail-open)"
// rationale: unlike a measured rate (a soft, optional enrichment to a
// PROPOSAL), this feeds a hard ceiling on a COMMIT that mutates real
// state -- this fleet's own rule is to fail loud for anything that
// mutates real state.
type InstalledCapacityClient interface {
	// InstalledCapacity returns the real count of stations registered
	// with pathId's capability in fulfillment-execution, or
	// ErrInstalledCapacityUnavailable on ANY failure to produce one --
	// unreachable service, malformed response, or a non-200. Unlike
	// MeasuredRateClient, there is no "genuinely no stations yet" case
	// to distinguish: fulfillment-execution's own endpoint already
	// treats zero registered stations for a capability as a real,
	// valid answer of 0, not an error -- so a real 0 here means
	// exactly that, and CommitShiftPlan must reject any plannedHeads
	// greater than 0 for that path.
	InstalledCapacity(ctx context.Context, pathId shared.PathId) (int, error)
}

// ErrInstalledCapacityUnavailable is returned by an InstalledCapacityClient
// implementation for every failure mode a caller cannot usefully act on
// differently: an unreachable fulfillment-execution, a malformed
// response, or a non-200 status. CommitShiftPlan's single
// error-handling branch depends on this being the ONLY error an
// InstalledCapacityClient ever returns, and treats it as FATAL to the
// commit (fail loud), unlike MeasuredRateClient's fail-open contract.
var ErrInstalledCapacityUnavailable = errors.New("installed capacity unavailable")

// EventPublisher publishes domain events raised by aggregates.
type EventPublisher interface {
	Publish(ctx context.Context, events ...shared.DomainEvent) error
}

// UnitOfWork brackets a use case's state changes and the domain events it
// raises so all of them commit or none does (ADR 0016, transactional
// outbox).
//
// Execute runs fn inside one atomic scope. Every Repo.Save and
// EventPublisher.Publish made with the ctx handed to fn is bound to that
// same scope: if fn returns an error the scope is rolled back and nothing
// — neither the aggregate rows nor the outbox rows — is visible afterwards.
//
// Adapters that have no transactional backing (the in-memory repos, the
// log publisher) need no implementation at all: use cases treat a nil
// UnitOfWork as "run the calls back to back", so they stay adapter-agnostic
// either way.
type UnitOfWork interface {
	Execute(ctx context.Context, fn func(ctx context.Context) error) error
}

// ProcessedEvents is the idempotency gate for at-least-once event consumption:
// it records which event ids have already been handled so a redelivery is a
// no-op. It is used by the analytics projector's inbound consumer; the OLTP
// write path does not depend on it.
type ProcessedEvents interface {
	// MarkProcessed records eventId if absent, returning true iff this call
	// newly recorded it (so the caller should process the event) and false if
	// it was already seen (a duplicate to skip).
	MarkProcessed(ctx context.Context, eventId string) (bool, error)
}

// Clock supplies the current time so domain logic never calls time.Now()
// directly.
type Clock interface {
	Now() time.Time
}

// PathCatalogue is the outbound port for the fleet's declared process-path
// catalogue — matches pathcatalog.Catalogue's own Lookup signature exactly,
// so *pathcatalog.Catalogue already satisfies this interface with no
// changes. Introduced so an alternative adapter (e.g. a Kafka-sourced
// catalogue, see internal/adapters/outbound/kafkacatalog) can be wired in
// wherever a *pathcatalog.Catalogue was previously required directly.
type PathCatalogue interface {
	Lookup(id string) (pathcatalog.PathDefinition, error)
}
