// Package labourplan holds this context's local copy of warehouse-planning's
// published labour requirement (ADR 0043): one Requirement per site, shift
// and path, the heads the demand requires. warehouse-planning computes it
// (its ADR 0023) and publishes it as LabourPlanCommitted; this context never
// computes it again, it only keeps the latest version it was told.
package labourplan

import (
	"errors"
	"fmt"
	"strings"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// ErrInvalidRequirement marks a requirement that breaks the published
// contract: a missing site, shift or path, negative heads, or a version below
// 1. It is a deterministic failure: the same message can never become valid.
var ErrInvalidRequirement = errors.New("invalid labour requirement")

// ErrRequirementNotFound is returned when no requirement has been received
// for a (site, shift, path). A proposal from the labour plan answers it as a
// 404, never as zero heads (ADR 0043 decision 2).
var ErrRequirementNotFound = errors.New("no labour plan has been published for this site, shift and path")

// HeadsSource says how warehouse-planning derived the planned heads: from a
// caller's request or from its forecast. Stored as received; an unknown value
// is kept, not refused, so an additive producer change never stalls the copy.
type HeadsSource string

// The heads sources warehouse-planning publishes today (its ADR 0023).
const (
	HeadsSourceRequest  HeadsSource = "request"
	HeadsSourceForecast HeadsSource = "forecast"
)

// Key identifies one requirement row. SiteCode is the facility-layout site
// code warehouse-planning publishes as site_id (ADR 0035); every field is
// matched exactly as received.
type Key struct {
	SiteCode string
	ShiftId  string
	PathId   shared.PathId
}

// Requirement is one row of the local copy: the heads the demand requires on
// a path for a site's shift, with the rate and hours warehouse-planning used
// and the version it published (+1 per accepted change of the key).
type Requirement struct {
	Key              Key
	PlannedHeads     int
	RateUnitsPerHour float64
	Hours            float64
	HeadsSource      HeadsSource
	Version          int
}

// Validate checks the rules this context relies on: the key is complete,
// heads are not negative and the version is at least 1. Every failure wraps
// ErrInvalidRequirement.
func (r Requirement) Validate() error {
	switch {
	case strings.TrimSpace(r.Key.SiteCode) == "", strings.TrimSpace(r.Key.ShiftId) == "", strings.TrimSpace(string(r.Key.PathId)) == "":
		return fmt.Errorf("%w: site, shift and path are required", ErrInvalidRequirement)
	case r.PlannedHeads < 0:
		return fmt.Errorf("%w: planned heads %d is negative", ErrInvalidRequirement, r.PlannedHeads)
	case r.Version < 1:
		return fmt.Errorf("%w: version %d is below 1", ErrInvalidRequirement, r.Version)
	}
	return nil
}

// Supersedes is the version guard of ADR 0043 decision 1: r replaces the
// stored row only when none exists yet (found is false) or r's version is
// greater than the stored one. An equal or lower version is a no-op, so a
// redelivered or reordered older message never rolls the copy back.
func (r Requirement) Supersedes(stored Requirement, found bool) bool {
	return !found || r.Version > stored.Version
}
