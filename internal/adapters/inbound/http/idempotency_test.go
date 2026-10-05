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

// ADR-0027: with an IdempotencyPool wired, exactly the two
// resource-creation POSTs require the header; every other route is
// untouched.
func TestRouter_IdempotencyPoolProtectsOnlyTheCreationRoutes(t *testing.T) {
	h := newTestHandler()
	h.IdempotencyPool = lazyIdempotencyPool(t)
	router := NewRouter(h, testLogger, "")

	for _, path := range []string{"/shift-plans", "/associates/assoc-1/assignments"} {
		rec := doRequest(t, router, http.MethodPost, path, map[string]any{})
		assertProblemDetails(t, rec, http.StatusBadRequest, "idempotency-key-required", path)
	}

	// Unprotected: reaches its handler (201 / 204), no idempotency 400.
	if rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{Certifications: []string{"pack"}}); rec.Code != http.StatusCreated {
		t.Fatalf("start-shift must not require Idempotency-Key, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/certifications", certifyRequest{Certification: "pick"}); rec.Code != http.StatusNoContent {
		t.Fatalf("certifications must not require Idempotency-Key, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Without an IdempotencyPool (in-memory dev/test) the creation routes work
// with no header at all.
func TestRouter_NilIdempotencyPoolLeavesCreationRoutesUnprotected(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{Certifications: []string{"pack"}})
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
