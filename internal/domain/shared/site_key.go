package shared

import (
	"errors"
	"strings"
)

// ErrMissingSiteKey is returned when a ShiftPlan surface received neither the
// canonical siteCode nor its deprecated alias buildingId.
var ErrMissingSiteKey = errors.New("siteCode is required (buildingId is accepted as a deprecated alias)")

// ErrConflictingSiteAndBuilding is returned when a request carries both
// siteCode and its deprecated alias buildingId with DIFFERENT values: they
// name the same thing, so two values are ambiguous (ADR 0035).
var ErrConflictingSiteAndBuilding = errors.New("siteCode and buildingId (a deprecated alias of siteCode) are both present with different values")

// ResolveSiteKey resolves the ShiftPlan key from the canonical siteCode and
// its deprecated alias buildingId (ADR 0035). Both name the same value, stored
// in the same column. Surrounding whitespace is trimmed and a blank value counts
// as absent. At least one is required; both present and equal is fine; both
// present and different is ErrConflictingSiteAndBuilding.
func ResolveSiteKey(siteCode, buildingId string) (string, error) {
	site := strings.TrimSpace(siteCode)
	building := strings.TrimSpace(buildingId)
	switch {
	case site == "" && building == "":
		return "", ErrMissingSiteKey
	case site == "":
		return building, nil
	case building == "" || building == site:
		return site, nil
	default:
		return "", ErrConflictingSiteAndBuilding
	}
}

// ResolveGapLookup resolves the plan lookup key and the associate scope of a
// staffing-gap read from the canonical siteCode and the deprecated alias
// buildingId (ADR 0035):
//
//   - siteCode only: siteCode is both the plan key and the associate scope.
//   - buildingId only (legacy): buildingId is the plan key and the gap stays
//     unscoped (behaviour identical to before the convergence; callers use ids
//     like "bldg-1" that are not Site codes).
//   - both: buildingId stays the plan key and siteCode the scope, exactly as
//     ADR 0034 defined it. The "different values are a 422" rule of ADR 0035
//     is deliberately NOT applied to gap reads yet; see ADR 0035, "Open item".
func ResolveGapLookup(siteCode, buildingId string) (planKey string, scope SiteCode, err error) {
	scope = NewSiteCode(siteCode)
	building := strings.TrimSpace(buildingId)
	switch {
	case building == "" && scope.IsUnscoped():
		return "", "", ErrMissingSiteKey
	case building == "":
		return string(scope), scope, nil
	default:
		return building, scope, nil
	}
}
