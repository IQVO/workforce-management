package http

import (
	"errors"
	"net/http"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// Local validation sentinels for request fields that have no dedicated
// domain value object (shiftId is a plain string on ShiftPlan; the plan key's
// own missing/conflict errors live in shared, ADR 0035).
// These stay in the HTTP adapter: they gate malformed requests before a use
// case is ever invoked, they do not change use-case or domain behavior.
var (
	errMissingShiftId = errors.New("shiftId is required")
	errMissingCharge  = errors.New("charge is required")
	// errNullBody rejects a literal JSON null request body (a no-op when
	// decoded into a value struct). It intentionally has no dedicated
	// categoryFor entry: categoryFor's 400-status fallback already maps
	// it to malformed-request-body, which is exactly what a null body is.
	errNullBody = errors.New("request body must be a JSON object, not null")
)

// problemErrorsURIBase is the base for this service's RFC 7807 "type" URIs.
// It does not need to resolve; it is an identifier unique per error
// category, per the RFC.
const problemErrorsURIBase = "https://errors.workforce-management.warehouse-systems.dev"

// statusFor maps a typed domain/application error to an HTTP status code.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, shared.ErrEmptyAssociateId),
		errors.Is(err, shared.ErrEmptyPathId),
		errors.Is(err, shared.ErrEmptyCertification),
		errors.Is(err, pathcatalog.ErrUnknownPath),
		errors.Is(err, shiftplan.ErrNoPathPlans),
		errors.Is(err, shiftplan.ErrMissingInstalledStations):
		return http.StatusBadRequest
	case errors.Is(err, associate.ErrAlreadyOnBreak),
		errors.Is(err, associate.ErrNotOnBreak),
		errors.Is(err, associate.ErrOnBreak),
		errors.Is(err, associate.ErrShiftEnded),
		errors.Is(err, associate.ErrMaxHoursExceeded),
		errors.Is(err, assignment.ErrCertificationRequired),
		errors.Is(err, shiftplan.ErrPlannedHeadsExceedInstalled),
		errors.Is(err, shiftplan.ErrExceedsInstalledCapacity),
		errors.Is(err, shiftplan.ErrPlannedHoursExceedCapacity):
		return http.StatusConflict
	case errors.Is(err, ports.ErrConcurrentModification):
		// A different writer committed a version this caller never
		// saw between its load and its Save (ADR 0021, optimistic
		// concurrency) -- 409, same status family as the domain
		// conflicts above, but its own distinct category/detail so a
		// caller can tell "re-fetch and retry" apart from a business
		// rule rejection.
		return http.StatusConflict
	case errors.Is(err, shared.ErrMissingSiteKey):
		return http.StatusBadRequest
	case errors.Is(err, shared.ErrConflictingSiteAndBuilding):
		// siteCode and its deprecated alias buildingId name the same value;
		// two different values are a well-formed but unprocessable request
		// (ADR 0035). 422, not 409: no resource state is in conflict.
		return http.StatusUnprocessableEntity
	case errors.Is(err, ports.ErrInstalledCapacityUnavailable):
		// A dependency-reachability failure, not a client validation
		// error: the request itself was well-formed, but this service
		// could not verify it against fulfillment-execution's real
		// Station registry. 503, not 409/500 -- signals "retry later",
		// distinct from both a request-shape problem and a genuine bug
		// in this service. See ADR-0014.
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// problemCategory is the fixed, category-level (type, title) pair for one
// class of error, per RFC 7807. slug becomes the last path segment of
// "type"; title is a fixed human summary — the dynamic err.Error() text
// goes in "detail" instead, at write time.
type problemCategory struct {
	slug  string
	title string
}

// problemCategoryCatalog is the ordered error→category catalog categoryFor
// walks: the first entry whose sentinel matches via errors.Is wins, exactly
// as the original switch sequenced them, so precedence is unchanged. It
// mirrors statusFor's error set exactly, plus the HTTP-layer validation
// sentinels that statusFor never sees.
var problemCategoryCatalog = []struct {
	sentinel error
	category problemCategory
}{
	{sentinel: ports.ErrNotFound, category: problemCategory{"resource-not-found", "Resource not found"}},
	{sentinel: shared.ErrEmptyAssociateId, category: problemCategory{"empty-associate-id", "Associate id must not be empty"}},
	{sentinel: shared.ErrEmptyPathId, category: problemCategory{"empty-path-id", "Path id must not be empty"}},
	{sentinel: pathcatalog.ErrUnknownPath, category: problemCategory{"unknown-path-id", "Unrecognized process-path id"}},
	{sentinel: shared.ErrEmptyCertification, category: problemCategory{"empty-certification", "Certification must not be empty"}},
	{sentinel: shiftplan.ErrNoPathPlans, category: problemCategory{"shift-plan-no-path-plans", "Shift plan must have at least one path plan line"}},
	{sentinel: shiftplan.ErrMissingInstalledStations, category: problemCategory{"shift-plan-missing-installed-stations", "Missing installed station count for path"}},
	// The slug stays `missing-building-id` (ADR 0035): clients that match it
	// keep working; only the title/detail now name siteCode, the canonical key.
	{sentinel: shared.ErrMissingSiteKey, category: problemCategory{"missing-building-id", "siteCode is required (buildingId is a deprecated alias)"}},
	{sentinel: shared.ErrConflictingSiteAndBuilding, category: problemCategory{"conflicting-site-and-building", "siteCode and its deprecated alias buildingId have different values"}},
	{sentinel: errMissingShiftId, category: problemCategory{"missing-shift-id", "shiftId is required"}},
	{sentinel: errMissingCharge, category: problemCategory{"missing-charge", "charge is required"}},
	{sentinel: associate.ErrAlreadyOnBreak, category: problemCategory{"associate-already-on-break", "Associate is already on break"}},
	{sentinel: associate.ErrNotOnBreak, category: problemCategory{"associate-not-on-break", "Associate is not on break"}},
	{sentinel: associate.ErrOnBreak, category: problemCategory{"associate-on-break", "Associate on break cannot be assigned"}},
	{sentinel: associate.ErrShiftEnded, category: problemCategory{"associate-shift-ended", "Associate shift has already ended"}},
	{sentinel: associate.ErrMaxHoursExceeded, category: problemCategory{"max-hours-exceeded", "Max hours per shift exceeded"}},
	{sentinel: assignment.ErrCertificationRequired, category: problemCategory{"certification-required", "Associate lacks the certification required for this path"}},
	{sentinel: shiftplan.ErrPlannedHeadsExceedInstalled, category: problemCategory{"planned-heads-exceed-installed", "Planned heads exceed installed stations for path"}},
	{sentinel: shiftplan.ErrExceedsInstalledCapacity, category: problemCategory{"exceeds-installed-capacity", "Planned heads exceed the live installed capacity reported by fulfillment-execution"}},
	{sentinel: shiftplan.ErrPlannedHoursExceedCapacity, category: problemCategory{"planned-hours-exceed-capacity", "Planned hours exceed capacity for planned heads within max hours per shift"}},
	{sentinel: ports.ErrInstalledCapacityUnavailable, category: problemCategory{"installed-capacity-unavailable", "Could not verify installed capacity against fulfillment-execution"}},
	{sentinel: ports.ErrConcurrentModification, category: problemCategory{"concurrent-modification", "The resource was modified by another request; reload and retry"}},
}

// categoryFor maps a typed error to its RFC 7807 category. It mirrors
// statusFor's error set exactly, plus the HTTP-layer validation sentinels
// that statusFor never sees (those are written with a known 400 status
// directly, bypassing statusFor). Errors matching nothing here fall back to
// a status-keyed generic category (malformed body for 400, internal error
// otherwise) so every response still gets a well-formed problem+json body.
func categoryFor(status int, err error) problemCategory {
	for _, entry := range problemCategoryCatalog {
		if errors.Is(err, entry.sentinel) {
			return entry.category
		}
	}
	if status == http.StatusBadRequest {
		return problemCategory{"malformed-request-body", "Malformed request body"}
	}
	return problemCategory{"internal-error", "Internal server error"}
}
