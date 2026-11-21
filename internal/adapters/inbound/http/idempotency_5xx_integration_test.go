//go:build integration

// Integration tests for the fleet rule "a 5xx (and a panic) is never stored
// under an Idempotency-Key": the unit of work rolls back, the key row with it,
// and a retry under the same key runs the handler again. They drive the REAL
// RequireIdempotencyKey over a real Postgres (testcontainers, as the sibling
// idempotency_integration_test.go) with a scripted fake handler, plus one
// route-level case through the real router (503 installed-capacity-unavailable).
package http_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inboundhttp "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/pgtx"
)

// probeHandler is a fake wrapped handler: it records how often (and how
// concurrently) it ran, writes one row through the bound transaction (so a
// rollback is observable) and answers answer(call).
type probeHandler struct {
	mu                       sync.Mutex
	calls, active, maxActive int
	answer                   func(call int) int
	enter                    func(call int)
}

func (p *probeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.active++
	p.maxActive = max(p.maxActive, p.active)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.active--; p.mu.Unlock() }()

	if p.enter != nil {
		p.enter(call)
	}
	if tx, ok := pgtx.TxFrom(r.Context()); ok {
		_, _ = tx.Exec(r.Context(), "INSERT INTO idem_probe (v) VALUES ($1)", fmt.Sprint(call))
	}
	status := p.answer(call)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"status":%d,"call":%d}`, status, call)
}

func (p *probeHandler) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func probeDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := idempotencyDB(t)
	if _, err := pool.Exec(context.Background(), "CREATE TABLE idem_probe (v text)"); err != nil {
		t.Fatalf("create idem_probe: %v", err)
	}
	return pool
}

func keyRows(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	return count(t, pool, "SELECT count(*) FROM idempotency_keys WHERE key = '"+key+"'")
}

func probeRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return count(t, pool, "SELECT count(*) FROM idem_probe")
}

func isReplayed(rec *httptest.ResponseRecorder) bool {
	return rec.Header().Get(inboundhttp.IdempotencyReplayedHeader) == "true"
}

// failsThenSucceeds answers status on the first call and 201 afterwards.
func failsThenSucceeds(status int) func(int) int {
	return func(call int) int {
		if call == 1 {
			return status
		}
		return http.StatusCreated
	}
}

func TestIdempotency_5xx_IsNeverStored_RetryRunsAgain_SuccessThenReplays(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			pool := probeDB(t)
			h := &probeHandler{answer: failsThenSucceeds(status)}
			mw := inboundhttp.RequireIdempotencyKey(pool)(h)
			key := fmt.Sprintf("five-xx-%d", status)

			first := postRoute(mw, "/x", key, `{"a":1}`)
			if first.Code != status || isReplayed(first) || !strings.Contains(first.Body.String(), `"call":1`) {
				t.Fatalf("first = %d replayed=%v body=%s; want the handler's own %d written to the client", first.Code, isReplayed(first), first.Body.String(), status)
			}
			if got := keyRows(t, pool, key); got != 0 {
				t.Fatalf("idempotency rows after a %d = %d, want 0 (a 5xx is never stored)", status, got)
			}
			if got := probeRows(t, pool); got != 0 {
				t.Fatalf("handler writes after a %d = %d, want 0 (rolled back with the key)", status, got)
			}

			retry := postRoute(mw, "/x", key, `{"a":1}`)
			if retry.Code != http.StatusCreated || isReplayed(retry) || h.Calls() != 2 {
				t.Fatalf("retry = %d replayed=%v calls=%d; want 201, not replayed, handler run again", retry.Code, isReplayed(retry), h.Calls())
			}
			if got := keyRows(t, pool, key); got != 1 {
				t.Fatalf("idempotency rows after the successful retry = %d, want 1", got)
			}

			again := postRoute(mw, "/x", key, `{"a":1}`)
			if again.Code != http.StatusCreated || !isReplayed(again) || again.Body.String() != retry.Body.String() || h.Calls() != 2 {
				t.Fatalf("third = %d replayed=%v calls=%d; want the stored 201 replayed with Idempotency-Replayed: true", again.Code, isReplayed(again), h.Calls())
			}
			if got := probeRows(t, pool); got != 1 {
				t.Fatalf("handler writes = %d, want 1 (only the successful run committed)", got)
			}
		})
	}
}

func TestIdempotency_5xx_DifferentBodyUnderSameKeyIsANewRequest(t *testing.T) {
	pool := probeDB(t)
	h := &probeHandler{answer: failsThenSucceeds(http.StatusServiceUnavailable)}
	mw := inboundhttp.RequireIdempotencyKey(pool)(h)

	if first := postRoute(mw, "/x", "five-xx-body", `{"a":1}`); first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first = %d, want 503", first.Code)
	}
	second := postRoute(mw, "/x", "five-xx-body", `{"a":2}`)
	if second.Code != http.StatusCreated || isReplayed(second) || h.Calls() != 2 {
		t.Fatalf("different body after a 5xx = %d replayed=%v calls=%d; want a new request (201, handler run), not a 409", second.Code, isReplayed(second), h.Calls())
	}
}

func TestIdempotency_4xx_IsStillStoredAndReplayed(t *testing.T) {
	pool := probeDB(t)
	h := &probeHandler{answer: func(int) int { return http.StatusUnprocessableEntity }}
	mw := inboundhttp.RequireIdempotencyKey(pool)(h)

	first := postRoute(mw, "/x", "four-xx", `{}`)
	second := postRoute(mw, "/x", "four-xx", `{}`)
	if first.Code != 422 || second.Code != 422 || isReplayed(first) || !isReplayed(second) || h.Calls() != 1 {
		t.Fatalf("first=%d second=%d replayed=%v/%v calls=%d; want a stored 422 replayed once", first.Code, second.Code, isReplayed(first), isReplayed(second), h.Calls())
	}
	if got := keyRows(t, pool, "four-xx"); got != 1 {
		t.Fatalf("idempotency rows = %d, want 1", got)
	}
	if got := probeRows(t, pool); got != 0 {
		t.Fatalf("handler writes behind a 4xx = %d, want 0 (savepoint rolled back, as before)", got)
	}
}

// The existing in-flight guard is Postgres' own unique-index lock: the second
// INSERT ... ON CONFLICT blocks until the first transaction resolves. When the
// first ends 5xx its row vanishes, so the second proceeds as a fresh request:
// it must neither replay the 5xx nor run beside the first.
func TestIdempotency_5xx_ConcurrentSameKey_SecondRunsAfterFirstNeverReplaysIt(t *testing.T) {
	pool := probeDB(t)
	entered, release := make(chan struct{}), make(chan struct{})
	h := &probeHandler{
		answer: failsThenSucceeds(http.StatusServiceUnavailable),
		enter: func(call int) {
			if call == 1 {
				close(entered)
				<-release
			}
		},
	}
	mw := inboundhttp.RequireIdempotencyKey(pool)(h)

	var first, second *httptest.ResponseRecorder
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); first = postRoute(mw, "/x", "five-xx-race", `{"a":1}`) }()
	<-entered
	go func() { defer wg.Done(); second = postRoute(mw, "/x", "five-xx-race", `{"a":1}`) }()
	time.Sleep(300 * time.Millisecond) // let the second block on the first's uncommitted key row
	if got := h.Calls(); got != 1 {
		t.Fatalf("handler calls while the first is in flight = %d, want 1 (the second must wait)", got)
	}
	close(release)
	wg.Wait()

	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first = %d, want 503", first.Code)
	}
	if second.Code != http.StatusCreated || isReplayed(second) {
		t.Fatalf("second = %d replayed=%v; want 201 from its own run, never a replayed 5xx", second.Code, isReplayed(second))
	}
	if h.maxActive != 1 || h.Calls() != 2 {
		t.Fatalf("maxActive=%d calls=%d; want never concurrent and run twice", h.maxActive, h.Calls())
	}
	if got := probeRows(t, pool); got != 1 {
		t.Fatalf("handler writes = %d, want 1 (the failed run rolled back)", got)
	}
}

func TestIdempotency_Panic_RetryRunsAgainAndSuccessIsStored(t *testing.T) {
	pool := probeDB(t)
	h := &probeHandler{answer: func(call int) int { return http.StatusCreated }, enter: func(call int) {
		if call == 1 {
			panic("boom")
		}
	}}
	mw := inboundhttp.RequireIdempotencyKey(pool)(h)

	func() {
		defer func() { _ = recover() }()
		postRoute(mw, "/x", "panic-retry", `{}`)
	}()
	if got := keyRows(t, pool, "panic-retry"); got != 0 {
		t.Fatalf("idempotency rows after a panic = %d, want 0", got)
	}
	retry := postRoute(mw, "/x", "panic-retry", `{}`)
	again := postRoute(mw, "/x", "panic-retry", `{}`)
	if retry.Code != 201 || isReplayed(retry) || !isReplayed(again) || h.Calls() != 2 {
		t.Fatalf("retry=%d again replayed=%v calls=%d; want run again, then stored and replayed", retry.Code, isReplayed(again), h.Calls())
	}
}

// flakyCapacity fails like a fulfillment-execution outage until healthy flips.
type flakyCapacity struct {
	mu      sync.Mutex
	healthy bool
}

func (f *flakyCapacity) InstalledCapacity(context.Context, shared.Capability) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.healthy {
		return 0, fmt.Errorf("%w: fulfillment-execution down", ports.ErrInstalledCapacityUnavailable)
	}
	return 1000, nil
}

// The real incident: POST /shift-plans while fulfillment-execution is down
// answers 503 installed-capacity-unavailable; it must not be replayed for 24 h
// once the dependency is back.
func TestIdempotency_5xx_ShiftPlanCapacityOutageIsNotReplayedAfterRecovery(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)
	capacity := &flakyCapacity{}
	fx.handler.CommitShiftPlan.InstalledCapacity = capacity

	first := postRoute(fx.router, "/shift-plans", "capacity-outage-1", commitBody)
	if first.Code != http.StatusServiceUnavailable || problemSlug(t, first.Body.Bytes()) != "installed-capacity-unavailable" {
		t.Fatalf("first = %d %s, want 503 installed-capacity-unavailable", first.Code, first.Body.String())
	}
	if got := keyRows(t, pool, "capacity-outage-1"); got != 0 {
		t.Fatalf("idempotency rows after the 503 = %d, want 0", got)
	}
	capacity.mu.Lock()
	capacity.healthy = true
	capacity.mu.Unlock()
	retry := postRoute(fx.router, "/shift-plans", "capacity-outage-1", commitBody)
	if retry.Code != http.StatusCreated || isReplayed(retry) {
		t.Fatalf("retry after recovery = %d replayed=%v (body %s); want a fresh 201", retry.Code, isReplayed(retry), retry.Body.String())
	}
	if got := count(t, pool, "SELECT count(*) FROM shift_plan"); got != 1 {
		t.Fatalf("shift_plan rows = %d, want 1", got)
	}
}
