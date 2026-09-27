package ports

import "errors"

// ErrNotFound is returned by a repository when the requested aggregate does
// not exist.
var ErrNotFound = errors.New("not found")

// ErrConcurrentModification is returned by AssociateRepo.Save /
// AssignmentRepo.Save when the row was modified by another writer between
// this caller's load and its Save (see ADR 0021, optimistic concurrency):
// the aggregate's loaded version no longer matches the row's current
// version. The caller must re-fetch and retry; it is never safe to retry
// the same in-memory aggregate blindly.
var ErrConcurrentModification = errors.New("aggregate was concurrently modified; reload and retry")

// ErrMeasuredRateUnavailable is returned by a MeasuredRateClient
// implementation for every failure mode a caller cannot usefully act on
// differently: an unreachable labor-performance, a malformed response, or
// a real 200 response reporting no measurable data yet for this path.
// ProposePathPlan's single error-handling branch depends on this being
// the ONLY error a MeasuredRateClient ever returns.
var ErrMeasuredRateUnavailable = errors.New("measured rate unavailable")
