//go:build integration

package http_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/usecases"
)

// The key is GLOBAL: the same key on another route (or with another method)
// is a reuse -> 409 idempotency-key-reused, not a fresh key.
func TestIdempotency_KeyIsGlobalAcrossRoutes_Returns409(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)

	if first := post(fx.router, "/shift-plans", "global-key-1", commitBody); first.Code != http.StatusCreated {
		t.Fatalf("first call status = %d (body: %s)", first.Code, first.Body.String())
	}
	seedAssociate(t, pool, "assoc-1", "pack")
	second := post(fx.router, "/associates/assoc-1/assignments", "global-key-1", assignBody)
	if second.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", second.Code, second.Body.String())
	}
	if slug := problemSlug(t, second.Body.Bytes()); slug != "idempotency-key-reused" {
		t.Fatalf("slug = %q, want idempotency-key-reused", slug)
	}
	if second.Header().Get("Idempotency-Replayed") != "" {
		t.Fatal("a reuse conflict is not a replay")
	}
}

func TestIdempotency_InvalidKey_Returns400(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)

	for name, key := range map[string]string{
		"too long":      strings.Repeat("k", 129),
		"non printable": "bad\x01key",
		"non ascii":     "chave-é",
	} {
		rec := post(fx.router, "/shift-plans", key, commitBody)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, rec.Code)
		}
		if slug := problemSlug(t, rec.Body.Bytes()); slug != "idempotency-key-invalid" {
			t.Fatalf("%s: slug = %q, want idempotency-key-invalid", name, slug)
		}
	}
	if got := count(t, pool, "SELECT count(*) FROM idempotency_keys"); got != 0 {
		t.Fatalf("idempotency_keys rows = %d, want 0", got)
	}
}

// Previously unkeyed routes (shift-starts, certifications, break-*, shift-ends)
// now require a key and replay on a retry.
func TestIdempotency_FormerlyUnkeyedRoutes_RequireKeyAndReplay(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)
	associates := postgres.NewAssociateRepo(pool)
	uow := postgres.NewUnitOfWork(pool)
	clock := idemClock(fx.handler.AssignLabor.Clock.Now())
	fx.handler.StartAssociateShift = &usecases.StartAssociateShift{Associates: associates, Events: fx.publisher, Clock: clock, UnitOfWork: uow}

	path := "/associates/assoc-9/shift-starts"
	body := `{"certifications":["pack"]}`
	if rec := post(fx.router, path, "", body); rec.Code != http.StatusBadRequest ||
		problemSlug(t, rec.Body.Bytes()) != "idempotency-key-required" {
		t.Fatalf("missing key: %d %s", rec.Code, rec.Body.String())
	}
	first := post(fx.router, path, "start-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d (%s)", first.Code, first.Body.String())
	}
	second := post(fx.router, path, "start-1", body)
	if second.Code != http.StatusCreated || second.Body.String() != first.Body.String() ||
		second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay differs: %d %q %v", second.Code, second.Body.String(), second.Header())
	}
}
