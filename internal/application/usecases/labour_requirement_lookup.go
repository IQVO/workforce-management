package usecases

import (
	"context"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// requiredHeadsFor reads the heads warehouse-planning published for one
// line of a shift plan (ADR 0043 decision 4): the requirement shown next to
// the commitment. nil when no copy is wired or no row exists, never a
// fabricated 0. The plan key is the site code (ADR 0035).
func requiredHeadsFor(ctx context.Context, reqs ports.LabourRequirements, siteCode, shiftId string, pathId shared.PathId) (*int, error) {
	if reqs == nil {
		return nil, nil
	}
	req, found, err := reqs.Find(ctx, labourplan.Key{SiteCode: siteCode, ShiftId: shiftId, PathId: pathId})
	if err != nil || !found {
		return nil, err
	}
	heads := req.PlannedHeads
	return &heads, nil
}

// RequiredHeads returns, per committed line of sp, the heads warehouse-planning
// published for it (ADR 0043 decision 4); a line without a published row has
// no entry. It is read AFTER the commit and never compared with it: committing
// fewer heads than required is a legitimate supply decision, so Execute never
// refuses a line for differing from the requirement.
func (uc *CommitShiftPlan) RequiredHeads(ctx context.Context, sp *shiftplan.ShiftPlan) (map[shared.PathId]int, error) {
	out := make(map[shared.PathId]int)
	for _, line := range sp.Lines() {
		heads, err := requiredHeadsFor(ctx, uc.Requirements, sp.SiteCode(), sp.ShiftId(), line.PathId)
		if err != nil {
			return nil, err
		}
		if heads != nil {
			out[line.PathId] = *heads
		}
	}
	return out, nil
}
