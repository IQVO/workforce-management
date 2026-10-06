package usecases

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// RateSourceCaller and RateSourceMeasured identify where the rate used by
// ProposePathPlan.Execute came from -- a caller-supplied plannedRate, or a
// measured rate fed back from labor-performance (feature: close-the-loop
// measured rate, ADR-0012). Surfaced on the response so a human is never
// left guessing why heads came back 0 or where the number came from.
const (
	RateSourceCaller   = "caller"
	RateSourceMeasured = "measured"
)

// DefaultIdleShareTrimThreshold is the idle share above which ProposePathPlan
// trims its proposed heads (idleness-as-staffing-signal). Applied whenever
// ProposePathPlan is constructed with IdleShareTrimThreshold <= 0 (the zero
// value), so a caller that omits it entirely -- every existing test, and any
// composition root that doesn't read IDLE_SHARE_TRIM_THRESHOLD -- still gets
// this sensible default rather than an accidental 0% threshold that would
// trim on any nonzero idle share.
const DefaultIdleShareTrimThreshold = 0.30

// secondsPerHour converts labor-performance's mean seconds-per-task into the
// per-head hourly rate (units per head per hour) the proposal arithmetic uses.
const secondsPerHour = 3600.0

// ProposePathPlan is a pure computation: heads needed to cover a path's
// charge at a planned rate. It persists nothing — a human must still
// commit the plan via CommitShiftPlan.
//
// plannedRate is now OPTIONAL: a caller passing <= 0 (not supplied) gets a
// real measured rate fed back from labor-performance via MeasuredRate
// (feature: close-the-loop measured rate, ADR-0012) when one is available
// for this path's task type. This is a SOFT enrichment, not a mutation of
// real state: any failure to obtain a measured rate — labor-performance
// unreachable, no TaskType mapping for this path, or genuinely no data
// yet — falls through to the existing zero-rate behavior (0 proposed
// heads) rather than failing the request. A caller-supplied plannedRate
// always wins outright; MeasuredRate is never consulted when one is given.
//
// Independently of the rate source, a high observed IDLE SHARE for this
// path's task type (idleness-as-staffing-signal) trims the proposed
// heads: associates already spending a large share of clocked time idle
// signal that fewer additional heads are needed to clear the same
// charge. This applies whether resolvedRate came from the caller or from
// MeasuredRate. FAIL-OPEN by the same discipline as MeasuredRate: no
// IdleShare wired, or ErrIdleShareUnavailable (no TaskType mapping, or no
// idle observation yet), applies NO trim -- the pre-existing heads
// computation is the safe default.
type ProposePathPlan struct {
	Events       ports.EventPublisher
	Clock        ports.Clock
	MeasuredRate ports.MeasuredRateClient
	// IdleShare supplies the observed idle share used for the trim
	// described above. nil (the zero value) disables trimming entirely --
	// every existing composition and test that doesn't wire it keeps the
	// pre-idle-share heads computation unchanged.
	IdleShare ports.IdleShareClient
	// IdleShareTrimThreshold is the idle share ProposePathPlan trims
	// against; a value <= 0 (including the zero value from an omitted
	// field) resolves to DefaultIdleShareTrimThreshold at Execute time.
	IdleShareTrimThreshold float64
	// UnitOfWork brackets the Publish (ADR 0016). This use case persists
	// nothing, so the scope is trivial, but wrapping it keeps every
	// publishing use case uniform: the outbox row commits on its own.
	UnitOfWork ports.UnitOfWork
}

// Execute computes proposed heads and publishes ShiftPlanProposed. It
// returns the heads (after any idle-share trim), the resolved rate
// actually used, which source that rate came from (RateSourceCaller or
// RateSourceMeasured), and trimReason -- a human-readable, auditable
// explanation of why heads were trimmed, or "" when no trim was applied.
func (uc *ProposePathPlan) Execute(ctx context.Context, buildingId string, pathId shared.PathId, charge, plannedRate float64) (heads int, resolvedRate float64, rateSource string, trimReason string, err error) {
	resolvedRate = plannedRate
	rateSource = RateSourceCaller

	if resolvedRate <= 0 && uc.MeasuredRate != nil {
		measured, mErr := uc.MeasuredRate.MeanActualSeconds(ctx, pathId)
		if mErr == nil {
			// meanActualSeconds is a DURATION per task, but the heads
			// arithmetic expects a per-head HOURLY rate (units per head
			// per hour, the same unit as a caller's plannedRate):
			// 3600 / seconds-per-task. A non-positive or non-finite
			// duration cannot be converted -- treat it exactly like
			// ErrMeasuredRateUnavailable (fail-open, zero rate).
			if measured > 0 && !math.IsInf(measured, 0) && !math.IsNaN(measured) {
				resolvedRate = secondsPerHour / measured
				rateSource = RateSourceMeasured
			}
		} else if !errors.Is(mErr, ports.ErrMeasuredRateUnavailable) {
			// A MeasuredRateClient must only ever return
			// ErrMeasuredRateUnavailable; anything else is a
			// programming error in the adapter, not a business
			// condition to swallow.
			return 0, 0, "", "", mErr
		}
		// ErrMeasuredRateUnavailable: fall through with resolvedRate
		// unchanged (still <= 0, still RateSourceCaller) -- the
		// existing zero-rate behavior (0 heads) below.
	}

	heads = shiftplan.ProposedHeads(charge, resolvedRate)

	heads, trimReason, err = uc.applyIdleShareTrim(ctx, pathId, heads)
	if err != nil {
		return 0, 0, "", "", err
	}

	event := shared.NewShiftPlanProposed(uc.Clock.Now(), buildingId, pathId, heads, resolvedRate)
	err = atomically(ctx, uc.UnitOfWork, func(ctx context.Context) error {
		return uc.Events.Publish(ctx, event)
	})
	if err != nil {
		return 0, 0, "", "", err
	}
	return heads, resolvedRate, rateSource, trimReason, nil
}

// applyIdleShareTrim trims heads when pathId's task type has an observed
// idle share exceeding the configured threshold, floored at 1 head
// minimum so a trim never proposes zero heads for real work. Returns the
// (possibly unchanged) heads, an audit trimReason (empty when no trim was
// applied), and an error only for a programming-error IdleShareClient
// response (anything other than ErrIdleShareUnavailable) -- fail-open on
// every other case, mirroring MeasuredRateClient's discipline exactly.
func (uc *ProposePathPlan) applyIdleShareTrim(ctx context.Context, pathId shared.PathId, heads int) (int, string, error) {
	if uc.IdleShare == nil || heads <= 0 {
		return heads, "", nil
	}

	share, err := uc.IdleShare.IdleSharePct(ctx, pathId)
	if err != nil {
		if errors.Is(err, ports.ErrIdleShareUnavailable) {
			return heads, "", nil
		}
		return 0, "", err
	}

	threshold := uc.IdleShareTrimThreshold
	if threshold <= 0 {
		threshold = DefaultIdleShareTrimThreshold
	}
	if share <= threshold {
		return heads, "", nil
	}

	trimmed := int(math.Ceil(float64(heads) * (1 - share)))
	if trimmed < 1 {
		trimmed = 1
	}
	if trimmed >= heads {
		// Nothing to trim in practice (can only happen at share <= 0,
		// which never exceeds a positive threshold, but guards against
		// a pathological threshold <= 0 override).
		return heads, "", nil
	}

	reason := fmt.Sprintf(
		"observed idle share %.1f%% for path %q exceeds the %.1f%% trim threshold; heads trimmed from %d to %d",
		share*100, pathId, threshold*100, heads, trimmed,
	)
	return trimmed, reason, nil
}
