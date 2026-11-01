package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lazyIdempotencyPool returns a pgxpool that never dials (pgxpool connects
// lazily). The tests below only exercise paths that return BEFORE the
// middleware touches the database (the missing-header 400 and unprotected
// routes); the transactional behaviour is covered against a real Postgres
// by idempotency_integration_test.go.
func lazyIdempotencyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRequireIdempotencyKey_MissingHeaderIsRejectedBeforeAnyDatabaseWork(t *testing.T) {
	called := false
	h := RequireIdempotencyKey(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/shift-plans", strings.NewReader(`{}`)))

	assertProblemDetails(t, rec, http.StatusBadRequest, "idempotency-key-required", "/shift-plans")
	if called {
		t.Fatal("the wrapped handler must never run without the header")
	}
}

// Fleet REST guideline 2.1: with an IdempotencyPool wired, EVERY POST
// requires the header; GETs are untouched.
func TestRouter_IdempotencyPoolProtectsEveryPost(t *testing.T) {
	h := newTestHandler()
	h.IdempotencyPool = lazyIdempotencyPool(t)
	router := NewRouter(h, testLogger, "")

	for _, path := range []string{
		"/shift-plans", "/associates/assoc-1/assignments", "/associates/assoc-1/shift-starts",
		"/associates/assoc-1/certifications", "/paths/pack/plan-proposals",
		"/associates/assoc-1/break-starts", "/associates/assoc-1/break-ends", "/associates/assoc-1/shift-ends",
	} {
		rec := doRequest(t, router, http.MethodPost, path, map[string]any{})
		assertProblemDetails(t, rec, http.StatusBadRequest, "idempotency-key-required", path)
	}
}

// Without an IdempotencyPool (in-memory dev/test) the creation routes work
// with no header at all.
func TestRouter_NilIdempotencyPoolLeavesCreationRoutesUnprotected(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	doRequest(t, router, http.MethodPost, "/associates/assoc-1/shift-starts", startShiftRequest{Certifications: []string{"pack"}})
	if rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/assignments", assignLaborRequest{PathId: "pack"}); rec.Code != http.StatusCreated {
		t.Fatalf("assignments without a pool = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// A browser SPA must be allowed to send the header (CORS preflight).
func TestCORS_AllowsIdempotencyKeyHeader(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	req := httptest.NewRequest(http.MethodOptions, "/shift-plans", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "idempotency-key,content-type")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	allow := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers"))
	if !strings.Contains(allow, "idempotency-key") {
		t.Fatalf("Access-Control-Allow-Headers = %q, want it to include Idempotency-Key", allow)
	}
}

func TestRequireIdempotencyKey_MalformedKeyIsRejected(t *testing.T) {
	h := RequireIdempotencyKey(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("must not run") }))
	for _, key := range []string{strings.Repeat("a", 129), "tab\there", "ç"} {
		req := httptest.NewRequest(http.MethodPost, "/shift-plans", strings.NewReader(`{}`))
		req.Header.Set(IdempotencyKeyHeader, key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assertProblemDetails(t, rec, http.StatusBadRequest, "idempotency-key-invalid", "/shift-plans")
	}
}

// TestRetiredRoutesAreGone: the pre-guideline command routes answer 404/405.
func TestRetiredRoutesAreGone(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	for _, path := range []string{
		"/associates/assoc-1/start-shift",
		"/associates/assoc-1/end-shift",
		"/associates/assoc-1/break/start",
		"/associates/assoc-1/break/end",
		"/paths/pack/plan/propose",
	} {
		rec := doRequest(t, router, http.MethodPost, path, map[string]any{})
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 404 or 405", path, rec.Code)
		}
	}
}
