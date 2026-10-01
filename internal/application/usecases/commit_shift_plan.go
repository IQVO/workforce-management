package usecases

import (
	"context"
	"errors"
	"fmt"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// ErrCommitShiftPlanNoCatalogue is a wiring defect, not a business rule:
// CommitShiftPlan cannot resolve a path to the station capabilities it
// requires without a PathCatalogue, and guessing (e.g. treating the path
// id itself as the capability) is precisely the bug ADR-0014's addendum
// closes. It surfaces as a 500, never as a silently-passing check.
var ErrCommitShiftPlanNoCatalogue = errors.New("commit shift plan: no process-path catalogue wired")

// CommitShiftPlan validates and commits a human-decided headcount split
// across paths for one building's shift. installedStations is supplied by
// the caller (Work Planning owns that number; this context has no
// dependency on that service, so the request carries it) and validated
// independently against plannedHeads.
//
// InstalledCapacity additionally fetches a LIVE ceiling from
// fulfillment-execution's real Station registry for every line, before
// ever calling into the domain aggregate. fulfillment-execution counts
// stations by CAPABILITY (lower-case "pick", "pack", ...), not by process
// path, so each line's path is first resolved through Catalogue to the
// capabilities the process-path catalogue declares it requires
// (requiredCapabilities), and the path's live ceiling is derived from
// those -- see installedCapacityForPath. Per this fleet's own rule ("fail
// loud for anything that mutates real state"), an unknown path or an
// ErrInstalledCapacityUnavailable on ANY line fails the WHOLE commit --
// unlike ProposePathPlan's MeasuredRateClient, there is no fallback path
// here. See ADR-0014 and its addendum.
type CommitShiftPlan struct {
	ShiftPlans        ports.ShiftPlanRepo
	Events            ports.EventPublisher
	Clock             ports.Clock
	InstalledCapacity ports.InstalledCapacityClient
	// Catalogue resolves a line's path id to the station capabilities
	// it requires (ADR-0013). Required: a nil Catalogue fails every
	// commit with ErrCommitShiftPlanNoCatalogue rather than guessing a
	// capability.
	Catalogue        ports.PathCatalogue
	MaxHoursPerShift float64
	// UnitOfWork brackets Save + Publish atomically (ADR 0016); nil = none.
	// The integration publisher re-reads the saved plan while encoding, so
	// the Publish MUST run inside the same scope as the Save.
	UnitOfWork ports.UnitOfWork
}

// Execute resolves every line's path to its required capabilities,
// fetches a live installed-capacity ceiling for each from
// fulfillment-execution, then commits the plan. An unknown path
// (pathcatalog.ErrUnknownPath) or any ErrInstalledCapacityUnavailable
// fails the entire commit -- see this struct's own doc comment for why
// this differs from ProposePathPlan's fail-open MeasuredRateClient policy.
func (uc *CommitShiftPlan) Execute(ctx context.Context, buildingId, shiftId string, lines []shiftplan.PathPlan, installedStations map[shared.PathId]int) (*shiftplan.ShiftPlan, error) {
	if uc.Catalogue == nil {
		return nil, ErrCommitShiftPlanNoCatalogue
	}

	installedCapacity := make(map[shared.PathId]int, len(lines))
	// Several paths can share a capability (e.g. "pick" and
	// "pick-zone-a" both require "pick"); fetch each capability once.
	byCapability := make(map[shared.Capability]int)
	for _, line := range lines {
		if _, ok := installedCapacity[line.PathId]; ok {
			continue // a path can appear on multiple lines in a malformed request; resolve each path once
		}
		capacity, err := uc.installedCapacityForPath(ctx, line.PathId, byCapability)
		if err != nil {
			return nil, err
		}
		installedCapacity[line.PathId] = capacity
	}

	sp, err := shiftplan.CommitShiftPlan(buildingId, shiftId, lines, installedStations, installedCapacity, uc.MaxHoursPerShift, uc.Clock.Now())
	if err != nil {
		return nil, err
	}
	err = atomically(ctx, uc.UnitOfWork, func(ctx context.Context) error {
		if err := uc.ShiftPlans.Save(ctx, sp); err != nil {
			return err
		}
		return uc.Events.Publish(ctx, sp.PullEvents()...)
	})
	if err != nil {
		return nil, err
	}
	return sp, nil
}

// installedCapacityForPath derives a path's live installed-capacity
// ceiling from the stations able to serve it: a station serves a path
// only if it holds EVERY capability the path requires, so the ceiling is
// the MINIMUM, over the path's required capabilities, of the stations
// registered with that capability. (fulfillment-execution's endpoint
// counts one capability at a time, so the minimum is the tightest bound
// it can give; for the fleet's paths, which each declare exactly one
// capability, it is exactly that capability's count.)
//
// A path declaring NO capabilities gets a ceiling of 0, never an
// unconstrained one: no station can be shown to serve it, and ADR-0014
// already fixes "nothing to check against" as a 0 ceiling (a missing
// entry is never skipped). Both catalogue sources (filecatalog and
// process-path-management) reject an empty requiredCapabilities set, so
// this is a defensive fail-closed rule rather than an expected path.
//
// memo caches per-capability answers across the lines of one commit.
func (uc *CommitShiftPlan) installedCapacityForPath(ctx context.Context, pathId shared.PathId, memo map[shared.Capability]int) (int, error) {
	def, err := uc.Catalogue.Lookup(string(pathId))
	if err != nil {
		return 0, fmt.Errorf("path %q: %w", pathId, err)
	}

	ceiling := 0
	for i, raw := range def.RequiredCapabilities {
		capability := shared.Capability(raw)
		count, ok := memo[capability]
		if !ok {
			count, err = uc.InstalledCapacity.InstalledCapacity(ctx, capability)
			if err != nil {
				return 0, err
			}
			memo[capability] = count
		}
		if i == 0 || count < ceiling {
			ceiling = count
		}
	}
	return ceiling, nil
}
