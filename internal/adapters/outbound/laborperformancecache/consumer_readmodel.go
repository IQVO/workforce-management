package laborperformancecache

import (
	"context"
	"fmt"
	"strings"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// runningMean tracks an incremental sum+count for one TaskType, so
// Mean() is O(1) regardless of how many tasks have ever been recorded.
type runningMean struct {
	sum   float64
	count int64
}

func (m runningMean) mean() (float64, bool) {
	if m.count == 0 {
		return 0, false
	}
	return m.sum / float64(m.count), true
}

// idleShareTotals tracks the two running sums the idle-share ratio is
// built from: idleSeconds (numerator, fed only by non-nil
// idle_seconds_before) and actualSeconds (denominator half, fed by
// every message regardless of idle observability). observed reports
// whether at least one message has ever contributed a non-nil
// idle_seconds_before for this TaskType -- a TaskType that only ever
// sees actual_seconds (no idle observation yet) must report
// ErrIdleShareUnavailable, not a fabricated 0 share.
type idleShareTotals struct {
	idleSeconds   float64
	actualSeconds float64
	observed      bool
}

// share computes idleSeconds / (idleSeconds + actualSeconds), the
// fraction of this TaskType's clocked time (task-doing plus idle
// waiting) that was idle. ok is false when no idle observation has ever
// been recorded for this TaskType, or the two sums are both zero
// (nothing to divide).
func (t idleShareTotals) share() (float64, bool) {
	if !t.observed {
		return 0, false
	}
	denom := t.idleSeconds + t.actualSeconds
	if denom <= 0 {
		return 0, false
	}
	return t.idleSeconds / denom, true
}

// taskTypeForPathId maps this context's lowercase PathId (e.g. "pack",
// "pick") onto labor-performance's uppercase TaskType enum (PICK, PACK,
// SLAM). This is the SAME mapping table
// internal/adapters/outbound/laborperformance's HTTP client uses in its
// own taskTypeForPathId — duplicated here rather than imported because
// that function is unexported in its own package (Go's package
// visibility), but it must and does stay byte-for-byte identical. Not
// every PathId has a TaskType counterpart (e.g. "stow", "hazmat" are
// workforce-management-only paths with no measurable task type in
// labor-performance) — those report ok=false so the caller can fail
// fast without a cache lookup.
func taskTypeForPathId(pathId shared.PathId) (taskType string, ok bool) {
	switch strings.ToUpper(string(pathId)) {
	case "PICK", "PACK", "SLAM":
		return strings.ToUpper(string(pathId)), true
	default:
		return "", false
	}
}

// MeanActualSeconds satisfies ports.MeasuredRateClient against this
// consumer's current in-memory running means. Returns
// ports.ErrMeasuredRateUnavailable (never any other error) on ANY
// failure to produce a real value -- pathId has no TaskType counterpart,
// or no TaskPerformanceRecorded message for that TaskType has been
// observed yet -- so ProposePathPlan's single error-handling branch
// never needs to distinguish those cases; all of them mean the same
// thing to a caller: fall back to a caller-supplied rate. This preserves
// the exact fail-open contract internal/adapters/outbound/laborperformance's
// HTTP client and PermissiveClient already implement.
func (c *Consumer) MeanActualSeconds(_ context.Context, pathId shared.PathId) (float64, error) {
	taskType, ok := taskTypeForPathId(pathId)
	if !ok {
		return 0, fmt.Errorf("%w: path %q has no labor-performance task type", ports.ErrMeasuredRateUnavailable, pathId)
	}

	c.mu.RLock()
	totals, tracked := c.totals[taskType]
	c.mu.RUnlock()
	if !tracked {
		return 0, fmt.Errorf("%w: no measured rate observed yet for task type %q", ports.ErrMeasuredRateUnavailable, taskType)
	}
	mean, ok := totals.mean()
	if !ok {
		return 0, fmt.Errorf("%w: no measured rate observed yet for task type %q", ports.ErrMeasuredRateUnavailable, taskType)
	}
	return mean, nil
}

// IdleSharePct satisfies ports.IdleShareClient against this consumer's
// current in-memory running idle-share totals. Returns
// ports.ErrIdleShareUnavailable (never any other error) on ANY failure
// to produce a real value -- pathId has no TaskType counterpart, or no
// TaskPerformanceRecorded message carrying a non-nil idle_seconds_before
// has been observed yet for that TaskType -- mirroring
// MeanActualSeconds's fail-open contract exactly, so both GetStaffingGap
// and ProposePathPlan can treat "no idle signal" uniformly regardless of
// why.
func (c *Consumer) IdleSharePct(_ context.Context, pathId shared.PathId) (float64, error) {
	taskType, ok := taskTypeForPathId(pathId)
	if !ok {
		return 0, fmt.Errorf("%w: path %q has no labor-performance task type", ports.ErrIdleShareUnavailable, pathId)
	}

	c.mu.RLock()
	totals, tracked := c.idleTotals[taskType]
	c.mu.RUnlock()
	if !tracked {
		return 0, fmt.Errorf("%w: no idle share observed yet for task type %q", ports.ErrIdleShareUnavailable, taskType)
	}
	share, ok := totals.share()
	if !ok {
		return 0, fmt.Errorf("%w: no idle share observed yet for task type %q", ports.ErrIdleShareUnavailable, taskType)
	}
	return share, nil
}
