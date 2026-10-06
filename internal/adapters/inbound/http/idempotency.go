package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/claudioed/workforce-management/internal/pgtx"
)

// IdempotencyKeyHeader is the request header a caller must supply on a
// route wrapped by RequireIdempotencyKey.
const IdempotencyKeyHeader = "Idempotency-Key"

// RequireIdempotencyKey is route-scoped (chi's r.With(...)) transactional
// idempotency-key middleware for a true resource-CREATION endpoint — today,
// POST /shift-plans and POST /associates/{id}/assignments (see router.go;
// it is deliberately NOT applied to the whole router). Ported from
// order-management (ADR 0023) and inventory-storage (ADR 0018); see
// docs/docs/adr/0027-idempotency-key-middleware.md in this repo for the
// header contract, the transactional design, the concurrency argument and
// the deliberate v1 scope.
//
// # The core correctness argument: a committed row's status_code is NEVER NULL
//
// idempotency_keys.status_code (and response_body/response_headers) start
// NULL on insert and are only ever set by the SAME transaction that
// inserted the row, via one UPDATE, immediately before that transaction
// commits (see the "fresh key" branch below). There is no third state, no
// "in progress" marker, no timeout, no 409-retry-later branch: a row is
// either
//
//  1. not committed at all (the inserting transaction rolled back — e.g.
//     the wrapped handler panicked, or the UPDATE/commit itself failed),
//     in which case no OTHER transaction can ever see it, or
//  2. committed, in which case status_code/response_body/response_headers
//     were ALREADY populated by the UPDATE that ran, in the same
//     transaction, before the COMMIT that made the row visible at all.
//
// So a reader that finds a row can rely on status_code being non-NULL with
// no polling, no waiting, and no "come back later" response.
//
// # The concurrency argument: Postgres' own unique-index lock does the serialization
//
// Two concurrent requests with the SAME key race on
// `INSERT ... ON CONFLICT (key) DO NOTHING`. Postgres serializes them at the
// primary-key unique index: the SECOND (and every later) inserter's
// statement BLOCKS until the FIRST inserter's transaction resolves (commit
// OR rollback). So by the time ANY transaction observes rowsAffected()==0,
// the ORIGINAL inserting transaction has unconditionally finished; if it
// committed, the row's outcome is fully populated; if it rolled back, the
// row does not exist and THIS call's own blocked INSERT succeeds instead
// (the "fresh key" branch). No polling loop, lock-retry budget or timeout is
// needed — the database's own MVCC/locking semantics ARE the
// synchronization primitive.
func RequireIdempotencyKey(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(IdempotencyKeyHeader)
			if key == "" {
				writeProblem(w, http.StatusBadRequest,
					problemCategory{"idempotency-key-required", "Idempotency-Key header is required"},
					"This endpoint creates a new resource on every call and requires a caller-supplied "+IdempotencyKeyHeader+" header so a retried request is never applied twice.",
					r.URL.Path)
				return
			}

			// The body is read into memory once, HASHED, and then restored
			// (io.NopCloser over a bytes.Reader) so downstream JSON decoding
			// in the real handler is completely unaffected.
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				writeProblem(w, http.StatusBadRequest,
					problemCategory{"malformed-request-body", "Malformed request body"},
					err.Error(), r.URL.Path)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			sum := sha256.Sum256(bodyBytes)
			requestHash := hex.EncodeToString(sum[:])

			ctx := r.Context()
			tx, err := pool.Begin(ctx)
			if err != nil {
				writeProblem(w, http.StatusInternalServerError, internalErrorProblem, err.Error(), r.URL.Path)
				return
			}

			tag, err := tx.Exec(ctx, `
				INSERT INTO idempotency_keys (key, method, path, request_hash)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (key) DO NOTHING
			`, key, r.Method, r.URL.Path, requestHash)
			if err != nil {
				_ = tx.Rollback(ctx)
				writeProblem(w, http.StatusInternalServerError, internalErrorProblem, err.Error(), r.URL.Path)
				return
			}

			if tag.RowsAffected() == 0 {
				// A row already exists — see the doc comment's concurrency
				// argument for why the transaction that inserted it is
				// GUARANTEED to have already resolved by this point. This
				// call's own transaction never wrote anything, so it is
				// rolled back before the plain, non-transactional read.
				_ = tx.Rollback(ctx)
				replayCachedResponse(w, r, pool, key, requestHash)
				return
			}

			runFreshRequest(w, r, ctx, tx, key, next)
		})
	}
}

// internalErrorProblem mirrors categoryFor's own default case — this
// middleware runs before any domain/application error exists to map, so it
// constructs the same category directly.
var internalErrorProblem = problemCategory{"internal-error", "Internal server error"}

// writeProblem writes an RFC 7807 (application/problem+json) response for a
// middleware-level failure that has no domain error to map through
// categoryFor.
func writeProblem(w http.ResponseWriter, status int, cat problemCategory, detail, instance string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problemDetails{
		Type:     problemErrorsURIBase + "/" + cat.slug,
		Title:    cat.title,
		Status:   status,
		Detail:   detail,
		Instance: instance,
	})
}

// storedResponseHeaders is the JSON wire shape written to
// idempotency_keys.response_headers — a plain encoding of http.Header
// (map[string][]string), so no information (multi-value headers included)
// is lost on the round trip through JSONB.
type storedResponseHeaders = http.Header

// replayCachedResponse handles the "a row already exists for this key"
// path. The transaction that originally inserted this key has definitely
// already resolved — commit or rollback — by the time this function runs,
// so a plain read (no transaction of its own) is sufficient and cannot race
// a half-written row.
func replayCachedResponse(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, key, requestHash string) {
	ctx := r.Context()

	var storedHash string
	var statusCode *int
	var responseBody []byte
	var responseHeadersRaw []byte
	err := pool.QueryRow(ctx, `
		SELECT request_hash, status_code, response_body, response_headers
		FROM idempotency_keys WHERE key = $1
	`, key).Scan(&storedHash, &statusCode, &responseBody, &responseHeadersRaw)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, internalErrorProblem, err.Error(), r.URL.Path)
		return
	}

	if storedHash != requestHash {
		writeProblem(w, http.StatusUnprocessableEntity,
			problemCategory{"idempotency-key-reused", "Idempotency-Key was already used with a different request"},
			"The Idempotency-Key header on this request was already used with a request that had a different body. "+
				"Use a new Idempotency-Key for a genuinely different request.",
			r.URL.Path)
		return
	}

	// storedHash == requestHash: a genuine retry (same key AND same body).
	// status_code is guaranteed non-NULL here (see the doc comment's
	// no-null-status-code-ever-committed invariant).
	if statusCode == nil {
		// Unreachable given the invariant; a defensive 500 rather than a
		// panic or a fabricated response if it is ever violated.
		writeProblem(w, http.StatusInternalServerError, internalErrorProblem,
			"idempotency row committed with no recorded outcome — this should be impossible", r.URL.Path)
		return
	}

	if len(responseHeadersRaw) > 0 {
		var headers storedResponseHeaders
		if err := json.Unmarshal(responseHeadersRaw, &headers); err == nil {
			for k, vs := range headers {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
		}
	}
	w.WriteHeader(*statusCode)
	_, _ = w.Write(responseBody)
}

// runFreshRequest handles the "genuinely new key" path: it calls next with a
// CAPTURING recorder (rather than the real http.ResponseWriter), persists
// the outcome and commits, and ONLY THEN copies the captured response onto
// the real http.ResponseWriter. This ordering — capture, persist+commit,
// THEN write to the real client — is what guarantees the
// no-null-status-code-ever-committed invariant.
func runFreshRequest(w http.ResponseWriter, r *http.Request, ctx context.Context, tx pgx.Tx, key string, next http.Handler) {
	// A panic from the wrapped handler must roll back this transaction — so
	// neither the idempotency row nor any domain write it made (the
	// aggregate Save, the outbox insert) survives — and then re-panic so the
	// outer chi Recoverer middleware still produces the service's normal 500
	// response. A panic's outcome is deliberately never cached: a retry after
	// a panic must re-attempt the real work, not replay a stored 500.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	// The savepoint scopes the wrapped handler's writes (see below).
	sp, err := tx.Begin(ctx)
	if err != nil {
		_ = tx.Rollback(ctx)
		writeProblem(w, http.StatusInternalServerError, internalErrorProblem, err.Error(), r.URL.Path)
		return
	}

	txCtx := pgtx.WithTx(ctx, sp)
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r.WithContext(txCtx))

	// Every NORMAL (non-panic) response is cached here, including a
	// business-logic error response (4xx from validation, etc.) — a
	// deliberate v1 simplification (ADR-0027): a client retrying the exact
	// same key+body deterministically gets the exact same answer. A caller
	// wanting a different outcome must use a new Idempotency-Key.
	//
	// But the DOMAIN writes of a non-2xx request must never be committed
	// with it: a use case can fail late (e.g. a 409 optimistic-concurrency
	// conflict on its second Save) after an earlier write in the same
	// request already ran, and because the wrapped handler joined THIS
	// transaction nothing else would undo that write. So the handler runs
	// inside a SAVEPOINT: a 2xx releases it (domain writes stay in the
	// transaction and commit together with the idempotency row), anything
	// else rolls back to it (domain writes and outbox rows vanish) while the
	// idempotency row itself — inserted BEFORE the savepoint — survives and
	// still records the failed outcome for replay.
	if rec.Code >= 200 && rec.Code < 300 {
		err = sp.Commit(ctx) // RELEASE SAVEPOINT
	} else {
		err = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		writeProblem(w, http.StatusInternalServerError, internalErrorProblem, err.Error(), r.URL.Path)
		return
	}

	headersJSON, err := json.Marshal(rec.Header())
	if err != nil {
		_ = tx.Rollback(ctx)
		writeProblem(w, http.StatusInternalServerError, internalErrorProblem, err.Error(), r.URL.Path)
		return
	}

	if _, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET status_code = $1, response_body = $2, response_headers = $3, completed_at = now()
		WHERE key = $4
	`, rec.Code, rec.Body.Bytes(), headersJSON, key); err != nil {
		_ = tx.Rollback(ctx)
		writeProblem(w, http.StatusInternalServerError, internalErrorProblem, err.Error(), r.URL.Path)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		writeProblem(w, http.StatusInternalServerError, internalErrorProblem, err.Error(), r.URL.Path)
		return
	}

	// Only after a successful commit does the real client see anything — the
	// recorder's headers, then status, then body, VERBATIM.
	for k, vs := range rec.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}
