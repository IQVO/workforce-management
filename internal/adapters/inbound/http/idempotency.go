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

	"github.com/claudioed/workforce-management/internal/idempotency"
	"github.com/claudioed/workforce-management/internal/pgtx"
)

// IdempotencyKeyHeader is the request header a caller must supply on a
// route wrapped by RequireIdempotencyKey.
const IdempotencyKeyHeader = "Idempotency-Key"

// IdempotencyReplayedHeader is set to "true" on every replayed answer and
// never on a first execution (fleet REST guideline 2.3/2.4).
const IdempotencyReplayedHeader = "Idempotency-Replayed"

// maxIdempotencyKeyLength is the IdempotencyKey field value's upper bound in
// apis/openapi.yaml.
const maxIdempotencyKeyLength = 128

// validIdempotencyKey reports whether key matches the IdempotencyKey field
// value in apis/openapi.yaml (ADR 0042): 1-128 printable ASCII characters
// (0x20-0x7E) whose first and last character are visible (0x21-0x7E), the
// schema's `^[!-~]([ -~]*[!-~])?$`. Over HTTP/1.1 the edge rule is already
// guaranteed by the stack, which strips OWS before this runs; checking it
// here keeps the middleware exact for any caller that hands it an unstripped
// value.
func validIdempotencyKey(key string) bool {
	if key == "" || len(key) > maxIdempotencyKeyLength {
		return false
	}
	if !visibleASCII(key[0]) || !visibleASCII(key[len(key)-1]) {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < ' ' || key[i] > '~' {
			return false
		}
	}
	return true
}

// visibleASCII reports whether b is a visible (non-space) printable ASCII byte.
func visibleASCII(b byte) bool { return b > ' ' && b <= '~' }

// RequireIdempotencyKey is route-scoped (chi's r.With(...)) transactional
// idempotency-key middleware wrapping EVERY POST route (see router.go;
// it is deliberately NOT applied to the whole router, GETs need no key).
// The key is global: the stored row is found by key alone and compared by
// fingerprint (method + path + sha256(body)). Ported from
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
//
// # What is stored: a 2xx and a FINAL/OUTCOME refusal, never a RETRYABLE one, a 5xx or a panic
//
// Every problem type carries a replay class on its catalogue entry (ADR
// 0044): FINAL answers are a function of the request alone and are stored
// and replayed; OUTCOME answers (none exist in this service — the savepoint
// below discards every refusal's writes) would commit the writes with the
// stored answer; RETRYABLE answers depend on state that can change, so they
// are released exactly like a 5xx. The class mark travels on the capture
// recorder: writeError sets it from the catalogue entry it resolved, and
// this middleware holds no list of slugs and no status ranges beyond 2xx
// and 5xx. A refusal that arrives UNMARKED (a handler writing a problem by
// hand, bypassing writeError) is released like RETRYABLE — the safe
// fallback, never a silently-stored refusal.
//
// A 5xx answer and a panic are NEVER stored: the transaction rolls back —
// the key row and the handler's writes with it — and a retry under the
// same key runs the handler again (see runFreshRequest). A concurrent
// same-key request that was blocked on the first one's key row therefore
// runs fresh once the first fails; it never replays the failure and never
// runs beside it.
//
// # The replay window: a row past idempotency.KeyTTL never answers (ADR 0044 addendum)
//
// idempotency.KeyTTL (24 h — ONE constant shared with the ADR-0028
// sweeper's purge through the neutral internal/idempotency package,
// because the fitness tests forbid either adapter importing the other)
// bounds how late a stored answer may serve a retry. On the existing-row
// branch the middleware first tries to CLAIM a row past the window — one
// UPDATE that refreshes created_at and clears the stored answer, inside
// the request transaction — and a claimed key EXECUTES as a fresh request
// under the same key instead of answering. The claim's predicate
// re-checks created_at under the row lock, so two concurrent replays of
// one expired key cannot both claim it: the loser sees the refreshed row
// and replays the claimer's answer; if the claimer's transaction rolls
// back (a 5xx or a RETRYABLE refusal), Postgres re-evaluates the
// predicate against the still-expired row and the waiter claims it — the
// same serialization argument the unique-index insert makes above. The
// plain read in replayCachedResponse carries the complement predicate
// (created_at > now() - TTL), so no code path can answer an expired row.
func RequireIdempotencyKey(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(IdempotencyKeyHeader)
			if key == "" {
				writeProblem(w, http.StatusBadRequest,
					idempotencyKeyRequiredProblem,
					"Every POST requires a caller-supplied "+IdempotencyKeyHeader+" header so a retried request is never applied twice.",
					r.URL.Path)
				return
			}
			if !validIdempotencyKey(key) {
				writeProblem(w, http.StatusBadRequest,
					idempotencyKeyInvalidProblem,
					IdempotencyKeyHeader+" must be 1-128 printable ASCII characters that start and end with a visible character.",
					r.URL.Path)
				return
			}

			// The body is read into memory once, HASHED, and then restored
			// (io.NopCloser over a bytes.Reader) so downstream JSON decoding
			// in the real handler is completely unaffected.
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				writeProblem(w, http.StatusBadRequest,
					malformedRequestBodyProblem,
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
				// GUARANTEED to have already resolved by this point. A row
				// past the replay window never answers (ADR 0044 addendum).
				replayOrClaim(w, r, pool, tx, key, requestHash, next)
				return
			}

			runFreshRequest(w, r, ctx, tx, key, next)
		})
	}
}

// writeProblem writes an RFC 7807 (application/problem+json) response for
// any problem this package answers without a domain error to map through
// categoryFor (the middleware's own key-contract problems, infra failures,
// GET-route paging). The categories come from errors.go, classified (ADR
// 0044); like writeError, this writer marks the capture recorder with the
// category's class, so any refusal written through a shared writer carries
// its class (today no production writeProblem call runs inside the capture;
// the mark keeps the two writers uniform).
func writeProblem(w http.ResponseWriter, status int, cat problemCategory, detail, instance string) {
	if cap, ok := w.(*captureRecorder); ok {
		cap.markedClass = cat.class
	}
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
// a half-written row. The read itself is age-guarded — created_at within
// idempotency.KeyTTL, the complement of the claim below — so it can never
// answer an expired row, even at the window boundary.
func replayCachedResponse(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, key, requestHash string) {
	ctx := r.Context()

	var storedHash, storedMethod, storedPath string
	var statusCode *int
	var responseBody []byte
	var responseHeadersRaw []byte
	err := pool.QueryRow(ctx, `
		SELECT request_hash, method, path, status_code, response_body, response_headers
		FROM idempotency_keys
		WHERE key = $1 AND created_at > now() - ($2 * interval '1 second')
	`, key, idempotency.KeyTTL.Seconds()).Scan(&storedHash, &storedMethod, &storedPath, &statusCode, &responseBody, &responseHeadersRaw)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, internalErrorProblem, err.Error(), r.URL.Path)
		return
	}

	if storedHash != requestHash || storedMethod != r.Method || storedPath != r.URL.Path {
		writeProblem(w, http.StatusConflict,
			idempotencyKeyReusedProblem,
			"The Idempotency-Key header on this request was already used with a different request (method, path or body). "+
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
	w.Header().Set(IdempotencyReplayedHeader, "true")
	w.WriteHeader(*statusCode)
	_, _ = w.Write(responseBody)
}

// replayOrClaim serves the "a row already exists for this key" branch with
// the request transaction still open. A row past the replay window
// (idempotency.KeyTTL) never answers (ADR 0044 addendum): it is CLAIMED —
// created_at refreshed, stored answer cleared — and this request EXECUTES
// fresh under the same key. A live row is replayed: this call's own
// transaction never wrote anything, so it is rolled back before the plain,
// non-transactional read.
func replayOrClaim(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, tx pgx.Tx, key, requestHash string, next http.Handler) {
	ctx := r.Context()
	claimed, err := claimExpiredKey(ctx, tx, r, key, requestHash)
	if err != nil {
		_ = tx.Rollback(ctx)
		writeProblem(w, http.StatusInternalServerError, internalErrorProblem, err.Error(), r.URL.Path)
		return
	}
	if claimed {
		runFreshRequest(w, r, ctx, tx, key, next)
		return
	}
	_ = tx.Rollback(ctx)
	replayCachedResponse(w, r, pool, key, requestHash)
}

// claimExpiredKey takes over an idempotency_keys row that has aged past the
// replay window (idempotency.KeyTTL) so THIS request executes fresh under
// the same key: created_at is refreshed to now() and any stored answer
// cleared, inside the caller's still-open request transaction (runFreshRequest
// then records the new outcome in the same commit). It reports whether the
// claim won — false means the row is still inside the window, or a
// concurrent replay already claimed it and committed (the predicate
// re-checks created_at under the row lock; see the middleware's doc
// comment), and the row must be replayed instead.
func claimExpiredKey(ctx context.Context, tx pgx.Tx, r *http.Request, key, requestHash string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET method = $2, path = $3, request_hash = $4,
		    status_code = NULL, response_body = NULL, response_headers = NULL,
		    completed_at = NULL, created_at = now()
		WHERE key = $1
		  AND created_at <= now() - ($5 * interval '1 second')
	`, key, r.Method, r.URL.Path, requestHash, idempotency.KeyTTL.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// captureRecorder is the recorder runFreshRequest captures the handler's
// answer in, extended with the replay-class mark writeError sets while
// writing a refusal through it (ADR 0044). An unmarked refusal (a handler
// that bypassed the shared error writer) reads as the empty class and is
// released like RETRYABLE — the middleware never stores what it cannot
// classify.
type captureRecorder struct {
	*httptest.ResponseRecorder
	markedClass replayClass
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
	rec := &captureRecorder{ResponseRecorder: httptest.NewRecorder()}
	next.ServeHTTP(rec, r.WithContext(txCtx))

	// A server failure is never stored (fleet idempotency rule, ADR-0027
	// addendum): a 5xx says "this attempt failed", not "this is the answer
	// for this key", so the retry the client is told to make must run the
	// real work again. Rolling back the WHOLE transaction takes the key row
	// and every write the handler made (aggregate, outbox) with it, which
	// also releases the in-flight guard: a second request with the same key
	// that is blocked on the key row's unique index then proceeds as a fresh
	// request instead of replaying the failure. The client still receives
	// the 5xx itself, verbatim.
	//
	// A RETRYABLE refusal (ADR 0044) is released by exactly the same
	// branch: nothing committed, and the answer depends on state that can
	// change (an optimistic-lock loss, a referenced fact an event or another
	// request may deliver later), so a stored copy would pin a stale refusal
	// for the key's TTL. An unmarked refusal is released too — the safe
	// fallback for any answer this package did not classify.
	is2xx := rec.Code >= 200 && rec.Code < 300
	released := rec.Code >= http.StatusInternalServerError ||
		(!is2xx && rec.markedClass != replayFinal && rec.markedClass != replayOutcome)
	if released {
		_ = tx.Rollback(ctx)
		writeRecorded(w, rec.ResponseRecorder)
		return
	}

	// Every remaining NORMAL response is cached here. A 2xx keeps its
	// writes; a FINAL refusal is a function of the request alone, so its
	// stored copy can never go stale; an OUTCOME refusal (no catalogue
	// entry of this service uses the class — see ADR 0044) would keep the
	// writes the command made, committing them together with the stored
	// answer.
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
	// still records the outcome for replay.
	if is2xx || rec.markedClass == replayOutcome {
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
	writeRecorded(w, rec.ResponseRecorder)
}

// writeRecorded copies the captured response onto the real client connection:
// headers, then status, then body, verbatim.
func writeRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, vs := range rec.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}
