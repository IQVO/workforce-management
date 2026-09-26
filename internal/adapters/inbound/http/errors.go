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
// domain value object (buildingId/shiftId are plain strings on ShiftPlan).
// These stay in the HTTP adapter: they gate malformed requests before a use
// case is ever invoked, they do not change use-case or domain behavior.
var (
	errMissingBuildingId = errors.New("buildingId is required")
	errMissingShiftId    = errors.New("shiftId is required")
	errMissingCharge     = errors.New("charge is required")
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

// categoryFor maps a typed error to its RFC 7807 category. It mirrors
// statusFor's error set exactly, plus the HTTP-layer validation sentinels
// that statusFor never sees (those are written with a known 400 status
// directly, bypassing statusFor). Errors matching nothing here fall back to
// a status-keyed generic category (malformed body for 400, internal error
// otherwise) so every response still gets a well-formed problem+json body.
func categoryFor(status int, err error) problemCategory {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return problemCategory{"resource-not-found", "Resource not found"}
	case errors.Is(err, shared.ErrEmptyAssociateId):
		return problemCategory{"empty-associate-id", "Associate id must not be empty"}
	case errors.Is(err, shared.ErrEmptyPathId):
		return problemCategory{"empty-path-id", "Path id must not be empty"}
	case errors.Is(err, pathcatalog.ErrUnknownPath):
		return problemCategory{"unknown-path-id", "Unrecognized process-path id"}
	case errors.Is(err, shared.ErrEmptyCertification):
		return problemCategory{"empty-certification", "Certification must not be empty"}
	case errors.Is(err, shiftplan.ErrNoPathPlans):
		return problemCategory{"shift-plan-no-path-plans", "Shift plan must have at least one path plan line"}
	case errors.Is(err, shiftplan.ErrMissingInstalledStations):
		return problemCategory{"shift-plan-missing-installed-stations", "Missing installed station count for path"}
	case errors.Is(err, errMissingBuildingId):
		return problemCategory{"missing-building-id", "buildingId is required"}
	case errors.Is(err, errMissingShiftId):
		return problemCategory{"missing-shift-id", "shiftId is required"}
	case errors.Is(err, errMissingCharge):
		return problemCategory{"missing-charge", "charge is required"}
	case errors.Is(err, associate.ErrAlreadyOnBreak):
		return problemCategory{"associate-already-on-break", "Associate is already on break"}
	case errors.Is(err, associate.ErrNotOnBreak):
		return problemCategory{"associate-not-on-break", "Associate is not on break"}
	case errors.Is(err, associate.ErrOnBreak):
		return problemCategory{"associate-on-break", "Associate on break cannot be assigned"}
	case errors.Is(err, associate.ErrShiftEnded):
		return problemCategory{"associate-shift-ended", "Associate shift has already ended"}
	case errors.Is(err, associate.ErrMaxHoursExceeded):
		return problemCategory{"max-hours-exceeded", "Max hours per shift exceeded"}
	case errors.Is(err, assignment.ErrCertificationRequired):
		return problemCategory{"certification-required", "Associate lacks the certification required for this path"}
	case errors.Is(err, shiftplan.ErrPlannedHeadsExceedInstalled):
		return problemCategory{"planned-heads-exceed-installed", "Planned heads exceed installed stations for path"}
	case errors.Is(err, shiftplan.ErrExceedsInstalledCapacity):
		return problemCategory{"exceeds-installed-capacity", "Planned heads exceed the live installed capacity reported by fulfillment-execution"}
	case errors.Is(err, shiftplan.ErrPlannedHoursExceedCapacity):
		return problemCategory{"planned-hours-exceed-capacity", "Planned hours exceed capacity for planned heads within max hours per shift"}
	case errors.Is(err, ports.ErrInstalledCapacityUnavailable):
		return problemCategory{"installed-capacity-unavailable", "Could not verify installed capacity against fulfillment-execution"}
	case status == http.StatusBadRequest:
		return problemCategory{"malformed-request-body", "Malformed request body"}
	default:
		return problemCategory{"internal-error", "Internal server error"}
	}
}
