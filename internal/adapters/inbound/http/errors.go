package http

import (
	"errors"
	"net/http"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
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

// replayClass is the fleet idempotency replay class of one problem type
// (.claude/rules/fleet/idempotency-and-outbox.md; ADR 0044): what a retry
// under the same Idempotency-Key does with that answer.
//
//   - FINAL: the answer is a function of the request alone. Stored and
//     replayed.
//   - OUTCOME: the command COMMITTED state and then answered an error that
//     must be remembered. Stored AND the writes commit with the stored
//     answer. (No entry in this service uses it: this store rolls every
//     non-2xx answer's writes back to a savepoint, so a refusal can never
//     commit state — see ADR 0044. The middleware implements it anyway.)
//   - RETRYABLE: nothing committed; the answer depends on state that can
//     change. NOT stored — the key is released and a retry executes again.
type replayClass string

const (
	replayFinal     replayClass = "FINAL"
	replayOutcome   replayClass = "OUTCOME"
	replayRetryable replayClass = "RETRYABLE"
)

// statusFor maps a typed domain/application error to an HTTP status code.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ports.ErrNotFound),
		errors.Is(err, labourplan.ErrRequirementNotFound):
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
// class of error, per RFC 7807, plus its idempotency replay class (ADR 0044).
// slug becomes the last path segment of "type"; title is a fixed human
// summary — the dynamic err.Error() text goes in "detail" instead, at write
// time. class says what a retry under the same Idempotency-Key does with the
// answer; a category without one is refused by
// TestEveryProblemCategoryDeclaresAReplayClass (there is no runtime default:
// the middleware treats an unmarked refusal as released, like RETRYABLE).
//
// Every problemCategory literal of this package lives in THIS file, so the
// one declaration point is also the one classification point
// (TestProblemCategoryLiteralsLiveOnlyInErrorsGo).
type problemCategory struct {
	slug  string
	title string
	class replayClass
}

// problemCategoryCatalog is the ordered error→category catalog categoryFor
// walks: the first entry whose sentinel matches via errors.Is wins, exactly
// as the original switch sequenced them, so precedence is unchanged. It
// mirrors statusFor's error set exactly, plus the HTTP-layer validation
// sentinels that statusFor never sees.
//
// The third field of every entry is its replay class (ADR 0044; the table
// with the code-cited reasoning per entry is that ADR, and
// TestADRClassTableMatchesTheCatalogue holds the two equal):
//
//   - FINAL: a function of the request alone (the emptiness/shape checks,
//     and the two capacity checks whose every operand the request itself
//     carries — installedStations is caller-supplied and maxHoursPerShift
//     is boot-time config).
//   - RETRYABLE: the answer reads state that another request or a consumed
//     event can change (the associate's shift/break/certification state,
//     the Kafka-fed labour plan and process-path catalogue, the live
//     fulfillment-execution capacity, an optimistic-lock loss) or is a 5xx.
//   - OUTCOME: none. This store rolls every non-2xx answer's writes back
//     to the request's savepoint, so no refusal can commit state.
var problemCategoryCatalog = []struct {
	sentinel error
	category problemCategory
}{
	{sentinel: ports.ErrNotFound, category: problemCategory{"resource-not-found", "Resource not found", replayRetryable}},
	{sentinel: labourplan.ErrRequirementNotFound, category: problemCategory{"labour-plan-not-found", "No labour plan has been published for this site, shift and path", replayRetryable}},
	{sentinel: shared.ErrEmptyAssociateId, category: problemCategory{"empty-associate-id", "Associate id must not be empty", replayFinal}},
	{sentinel: shared.ErrEmptyPathId, category: problemCategory{"empty-path-id", "Path id must not be empty", replayFinal}},
	{sentinel: pathcatalog.ErrUnknownPath, category: problemCategory{"unknown-path-id", "Unrecognized process-path id", replayRetryable}},
	{sentinel: shared.ErrEmptyCertification, category: problemCategory{"empty-certification", "Certification must not be empty", replayFinal}},
	{sentinel: shiftplan.ErrNoPathPlans, category: problemCategory{"shift-plan-no-path-plans", "Shift plan must have at least one path plan line", replayFinal}},
	{sentinel: shiftplan.ErrMissingInstalledStations, category: problemCategory{"shift-plan-missing-installed-stations", "Missing installed station count for path", replayFinal}},
	// The slug stays `missing-building-id` (ADR 0035): clients that match it
	// keep working; only the title/detail now name siteCode, the canonical key.
	{sentinel: shared.ErrMissingSiteKey, category: problemCategory{"missing-building-id", "siteCode is required (buildingId is a deprecated alias)", replayFinal}},
	{sentinel: shared.ErrConflictingSiteAndBuilding, category: problemCategory{"conflicting-site-and-building", "siteCode and its deprecated alias buildingId have different values", replayFinal}},
	{sentinel: errMissingShiftId, category: problemCategory{"missing-shift-id", "shiftId is required", replayFinal}},
	{sentinel: errMissingCharge, category: problemCategory{"missing-charge", "charge is required", replayFinal}},
	{sentinel: associate.ErrAlreadyOnBreak, category: problemCategory{"associate-already-on-break", "Associate is already on break", replayRetryable}},
	{sentinel: associate.ErrNotOnBreak, category: problemCategory{"associate-not-on-break", "Associate is not on break", replayRetryable}},
	{sentinel: associate.ErrOnBreak, category: problemCategory{"associate-on-break", "Associate on break cannot be assigned", replayRetryable}},
	{sentinel: associate.ErrShiftEnded, category: problemCategory{"associate-shift-ended", "Associate shift has already ended", replayRetryable}},
	{sentinel: associate.ErrMaxHoursExceeded, category: problemCategory{"max-hours-exceeded", "Max hours per shift exceeded", replayRetryable}},
	{sentinel: assignment.ErrCertificationRequired, category: problemCategory{"certification-required", "Associate lacks the certification required for this path", replayRetryable}},
	{sentinel: shiftplan.ErrPlannedHeadsExceedInstalled, category: problemCategory{"planned-heads-exceed-installed", "Planned heads exceed installed stations for path", replayFinal}},
	{sentinel: shiftplan.ErrExceedsInstalledCapacity, category: problemCategory{"exceeds-installed-capacity", "Planned heads exceed the live installed capacity reported by fulfillment-execution", replayRetryable}},
	{sentinel: shiftplan.ErrPlannedHoursExceedCapacity, category: problemCategory{"planned-hours-exceed-capacity", "Planned hours exceed capacity for planned heads within max hours per shift", replayFinal}},
	{sentinel: ports.ErrInstalledCapacityUnavailable, category: problemCategory{"installed-capacity-unavailable", "Could not verify installed capacity against fulfillment-execution", replayRetryable}},
	{sentinel: ports.ErrConcurrentModification, category: problemCategory{"concurrent-modification", "The resource was modified by another request; reload and retry", replayRetryable}},
}

// The middleware's own problems and the writer's fallbacks are problem types
// of the idempotent routes too, so they are declared here, classified, next
// to the catalogue (ADR 0044). None of them is ever STORED as a key's
// outcome in production: the key-contract 400s and the body-read 400 are
// answered before any transaction exists, key-reused is computed on the
// replay path, and internal-error is a 5xx (never stored, ADR 0027
// addendum). Their classes still declare what a retry does.
var (
	idempotencyKeyRequiredProblem = problemCategory{"idempotency-key-required", "Idempotency-Key header is required", replayFinal}
	idempotencyKeyInvalidProblem  = problemCategory{"idempotency-key-invalid", "Idempotency-Key header is invalid", replayFinal}
	idempotencyKeyReusedProblem   = problemCategory{"idempotency-key-reused", "Idempotency-Key was already used with a different request", replayRetryable}
	malformedRequestBodyProblem   = problemCategory{"malformed-request-body", "Malformed request body", replayFinal}
	internalErrorProblem          = problemCategory{"internal-error", "Internal server error", replayRetryable}
)

// Off-keyed-route problems (collection GET paging, readiness): classified for
// the same "no unclassified problem type" rule; their routes never open a
// key transaction, so the class is declarative only.
var (
	unknownQueryParameterProblem = problemCategory{"unknown-query-parameter", "Unknown query parameter", replayFinal}
	invalidLimitProblem          = problemCategory{"invalid-limit", "Invalid limit", replayFinal}
	invalidCursorProblem         = problemCategory{"invalid-cursor", "Invalid cursor", replayFinal}
	dependencyUnavailableProblem = problemCategory{"dependency-unavailable", "A runtime dependency is unavailable", replayRetryable}
)

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
		return malformedRequestBodyProblem
	}
	return internalErrorProblem
}
