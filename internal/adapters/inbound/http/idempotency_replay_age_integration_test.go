//go:build integration

// The replay age guard (ADR 0044 addendum): between a row's expiry
// (idempotency.KeyTTL, the constant the ADR-0028 sweeper also purges on)
// and the sweeper's hourly pass, a replay of the same key+body must
// EXECUTE the handler again — never answer from the expired row — while a
// row still inside the window keeps answering. Seeded rows make the two
// outcomes distinguishable without waiting 24 h: the stored answer is a
// fabricated body no real handler would produce.
package http_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inboundhttp "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/idempotency"
)

// seedKeyRow inserts an idempotency_keys row for POST /shift-plans with the
// given body's fingerprint and a fabricated 201 answer, created_at age ago.
func seedKeyRow(t *testing.T, pool *pgxpool.Pool, key, body string, age time.Duration) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	stored := []byte(`{"stored":"` + key + `-answer-never-produced-by-any-handler"}`)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO idempotency_keys
			(key, method, path, request_hash, status_code, response_body, response_headers, created_at, completed_at)
		VALUES ($1, 'POST', '/shift-plans', $2, 201, $3, NULL, now() - ($4 * interval '1 second'), now() - ($4 * interval '1 second'))
	`, key, hex.EncodeToString(sum[:]), stored, age.Seconds()); err != nil {
		t.Fatalf("seed idempotency_keys row: %v", err)
	}
}

// An expired row must not answer: the replay EXECUTES the real handler
// under the same key (no Idempotency-Replayed header, the handler's own
// answer, one plan, one publish), the row is refreshed — created_at back
// inside the window, the new answer stored — so a further replay of the
// same key answers the NEW execution.
func TestIdempotency_Replay_ExpiredKeyExecutesInsteadOfAnswering(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)
	seedKeyRow(t, pool, "age-expired-1", commitBody, idempotency.KeyTTL+time.Hour)

	rec := postRoute(fx.router, "/shift-plans", "age-expired-1", commitBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (body: %s), want 201: the expired key must EXECUTE", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(inboundhttp.IdempotencyReplayedHeader); got != "" {
		t.Fatalf("Idempotency-Replayed = %q, want absent: an expired row must not answer", got)
	}
	if want := `{"stored":"age-expired-1-answer-never-produced-by-any-handler"}`; rec.Body.String() == want {
		t.Fatal("the fabricated stored answer was replayed; the handler must have executed instead")
	}
	if got := count(t, pool, "SELECT count(*) FROM shift_plan"); got != 1 {
		t.Fatalf("shift_plan rows = %d, want 1 (a real execution)", got)
	}
	if got := fx.publisher.Count(); got != 1 {
		t.Fatalf("publish count = %d, want 1 (a real execution)", got)
	}

	// The executed answer replaced the expired one and the window restarted.
	var createdAt time.Time
	var storedBody []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT created_at, response_body FROM idempotency_keys WHERE key = $1`, "age-expired-1",
	).Scan(&createdAt, &storedBody); err != nil {
		t.Fatalf("read the claimed row: %v", err)
	}
	if !createdAt.After(time.Now().Add(-idempotency.KeyTTL)) {
		t.Fatalf("created_at = %s, want refreshed inside the %s window", createdAt, idempotency.KeyTTL)
	}
	if string(storedBody) != rec.Body.String() {
		t.Fatalf("stored response_body = %q, want the executed answer %q", storedBody, rec.Body.String())
	}

	// A further replay of the now-live row answers the NEW execution.
	again := postRoute(fx.router, "/shift-plans", "age-expired-1", commitBody)
	if again.Code != http.StatusCreated || again.Header().Get(inboundhttp.IdempotencyReplayedHeader) != "true" {
		t.Fatalf("replay after re-execution = %d replayed=%q, want the stored 201 replayed",
			again.Code, again.Header().Get(inboundhttp.IdempotencyReplayedHeader))
	}
	if again.Body.String() != rec.Body.String() || fx.publisher.Count() != 1 {
		t.Fatalf("the further replay re-executed (publishes = %d); it must answer the refreshed row", fx.publisher.Count())
	}
}

// A row still inside the window keeps answering: the guard must not
// over-fire and turn live replays into executions.
func TestIdempotency_Replay_LiveRowStillAnswers(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)
	seedKeyRow(t, pool, "age-live-1", commitBody, idempotency.KeyTTL-time.Hour)

	rec := postRoute(fx.router, "/shift-plans", "age-live-1", commitBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (body: %s), want the stored 201", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(inboundhttp.IdempotencyReplayedHeader); got != "true" {
		t.Fatalf("Idempotency-Replayed = %q, want true: a live row still answers", got)
	}
	if want := `{"stored":"age-live-1-answer-never-produced-by-any-handler"}`; rec.Body.String() != want {
		t.Fatalf("body = %s, want the stored answer verbatim", rec.Body.String())
	}
	if got := fx.publisher.Count(); got != 0 {
		t.Fatalf("publish count = %d, want 0 (no execution for a live row)", got)
	}
	if got := count(t, pool, "SELECT count(*) FROM shift_plan"); got != 0 {
		t.Fatalf("shift_plan rows = %d, want 0 (no execution for a live row)", got)
	}
}
