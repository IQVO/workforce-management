package http

import (
	"errors"
	"net/http"
)

// This file exists only for the replay-class template test rendered in this
// package's external test package (idempotency_replay_classes_integration_test.go,
// from warehouse-harness-template): it exposes the production catalogue and
// a refusal through the production error writers, so the template's
// assertions drive the real class marking instead of a copy of it.

// errReplayProbe is the detail text of a probe refusal written through the
// production writers.
var errReplayProbe = errors.New("replay-class probe")

// statusForProblemType is the status a catalogue sentinel is answered with
// on the wire. It is statusFor for every domain sentinel, plus the explicit
// 400 the HTTP-layer validation sentinels (errMissingShiftId,
// errMissingCharge) are always written with — statusFor has no case for
// them because handlers pass the status directly (errors.go). These two are
// package-local sentinels compared by identity (never wrapped), so == is
// exact here.
func statusForProblemType(err error) int {
	if err == errMissingShiftId || err == errMissingCharge {
		return http.StatusBadRequest
	}
	return statusFor(err)
}

// ProblemTypeForTest is one problem type of the production catalogue as the
// replay-class template test needs it (Slug, the status it is answered
// with, and the replay class its catalogue entry declares).
type ProblemTypeForTest struct {
	Slug   string
	Status int
	Class  string
}

// ReplayCatalogueForTest exposes every problem type the idempotent routes
// can answer, with its declared replay class: the domain catalogue plus the
// middleware/writer-owned problems and the fallbacks (ADR 0044). The
// off-keyed-route problems (paging, readiness) are deliberately NOT here:
// they cannot occur on a keyed route.
func ReplayCatalogueForTest() []ProblemTypeForTest {
	out := make([]ProblemTypeForTest, 0, len(problemCategoryCatalog)+5)
	for _, e := range problemCategoryCatalog {
		out = append(out, ProblemTypeForTest{Slug: e.category.slug, Status: statusForProblemType(e.sentinel), Class: string(e.category.class)})
	}
	out = append(out,
		ProblemTypeForTest{Slug: idempotencyKeyRequiredProblem.slug, Status: http.StatusBadRequest, Class: string(idempotencyKeyRequiredProblem.class)},
		ProblemTypeForTest{Slug: idempotencyKeyInvalidProblem.slug, Status: http.StatusBadRequest, Class: string(idempotencyKeyInvalidProblem.class)},
		ProblemTypeForTest{Slug: idempotencyKeyReusedProblem.slug, Status: http.StatusConflict, Class: string(idempotencyKeyReusedProblem.class)},
		ProblemTypeForTest{Slug: malformedRequestBodyProblem.slug, Status: http.StatusBadRequest, Class: string(malformedRequestBodyProblem.class)},
		ProblemTypeForTest{Slug: internalErrorProblem.slug, Status: http.StatusInternalServerError, Class: string(internalErrorProblem.class)},
	)
	return out
}

// RefuseForTest answers slug exactly as production does — through the
// production error writers (writeError for a domain sentinel, writeProblem
// for the middleware's own problems), so the catalogue entry's class mark
// travels on the capture recorder. An unknown slug is answered through
// writeError's fallback path (a 500 internal-error).
func RefuseForTest(w http.ResponseWriter, r *http.Request, slug string) {
	for _, e := range problemCategoryCatalog {
		if e.category.slug == slug {
			writeError(w, r, statusForProblemType(e.sentinel), e.sentinel)
			return
		}
	}
	switch slug {
	case idempotencyKeyRequiredProblem.slug:
		writeProblem(w, http.StatusBadRequest, idempotencyKeyRequiredProblem, errReplayProbe.Error(), r.URL.Path)
	case idempotencyKeyInvalidProblem.slug:
		writeProblem(w, http.StatusBadRequest, idempotencyKeyInvalidProblem, errReplayProbe.Error(), r.URL.Path)
	case idempotencyKeyReusedProblem.slug:
		writeProblem(w, http.StatusConflict, idempotencyKeyReusedProblem, errReplayProbe.Error(), r.URL.Path)
	case malformedRequestBodyProblem.slug:
		writeError(w, r, http.StatusBadRequest, errReplayProbe)
	default:
		writeError(w, r, http.StatusInternalServerError, errReplayProbe)
	}
}
