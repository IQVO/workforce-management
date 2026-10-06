//go:build integration

// Integration tests for the transactional Idempotency-Key middleware
// (docs/docs/adr/0027-idempotency-key-middleware.md) against a real
// Postgres 16, through the REAL chi router (inboundhttp.NewRouter) over real
// net/http requests — not the middleware's internals in isolation.
// Testcontainers-only: the test boots and owns its own disposable Postgres,
// never reads DATABASE_URL or hardcodes localhost, so CI cannot silently
// skip this contract. Ported from order-management (ADR 0023) and
// inventory-storage (ADR 0018) and covering BOTH of this service's protected
// routes: POST /shift-plans and POST /associates/{id}/assignments.
package http_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	inboundhttp "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/pgtx"
)

// idempotencyDB boots a throwaway Postgres (testcontainers — the test owns
// its own database, never an external DATABASE_URL) and runs every
// migration in this repo, including the idempotency_keys one.
func idempotencyDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("workforce"),
		tcpostgres.WithUsername("workforce"),
		tcpostgres.WithPassword("workforce"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := postgres.Migrate(url, "../../../../migrations"); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	pool, err := postgres.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// countingPublisher wraps a real ports.EventPublisher and counts how many
// times Publish is actually invoked — the "was the real handler only
// executed once" proxy.
type countingPublisher struct {
	mu    sync.Mutex
	inner ports.EventPublisher
	count int
}

func (p *countingPublisher) Publish(ctx context.Context, evts ...shared.DomainEvent) error {
	p.mu.Lock()
	p.count++
	p.mu.Unlock()
	return p.inner.Publish(ctx, evts...)
}

func (p *countingPublisher) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

type idemClock time.Time

func (c idemClock) Now() time.Time { return time.Time(c) }

type unlimitedCapacity struct{}

func (unlimitedCapacity) InstalledCapacity(context.Context, shared.Capability) (int, error) {
	return math.MaxInt32, nil
}

type idempotencyFixture struct {
	router    http.Handler
	pool      *pgxpool.Pool
	publisher *countingPublisher
	handler   *inboundhttp.Handler
}

// newIdempotencyFixture wires the REAL chi router with Postgres-backed
// repos, UnitOfWork and the transactional OutboxPublisher (both Kafka
// publishers as writer-less Encoders, exactly as cmd/mcp does), so the full
// request cycle — idempotency bookkeeping, the use case's Save, the outbox
// insert — runs through the exact transaction-join mechanism production
// uses (internal/pgtx via postgres.UnitOfWork.Execute).
func newIdempotencyFixture(t *testing.T, pool *pgxpool.Pool) idempotencyFixture {
	t.Helper()
	associates := postgres.NewAssociateRepo(pool)
	shiftPlans := postgres.NewShiftPlanRepo(pool)
	assignments := postgres.NewAssignmentRepo(pool)
	uow := postgres.NewUnitOfWork(pool)
	outbox := postgres.NewOutboxPublisher(pool,
		outboundkafka.NewPublisherWithWriter(nil, shiftPlans),
		outboundkafka.NewAnalyticsPublisherWithWriter(nil, outboundkafka.NewEventID))
	publisher := &countingPublisher{inner: outbox}
	clock := idemClock(time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC))
	catalogue := pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
	})

	h := &inboundhttp.Handler{
		CommitShiftPlan: &usecases.CommitShiftPlan{
			ShiftPlans: shiftPlans, Events: publisher, Clock: clock, InstalledCapacity: unlimitedCapacity{},
			Catalogue: catalogue, MaxHoursPerShift: 8, UnitOfWork: uow,
		},
		AssignLabor: &usecases.AssignLabor{
			Associates: associates, Assignments: assignments, Events: publisher, Clock: clock,
			MaxHoursPerShift: 8, UnitOfWork: uow,
		},
		Catalogue:       catalogue,
		IdempotencyPool: pool,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return idempotencyFixture{router: inboundhttp.NewRouter(h, logger, ""), pool: pool, publisher: publisher, handler: h}
}

func seedAssociate(t *testing.T, pool *pgxpool.Pool, id string, certs ...string) {
	t.Helper()
	cc := make([]shared.Certification, len(certs))
	for i, c := range certs {
		cc[i] = shared.Certification(c)
	}
	shift := associate.NewAssociateShift(shared.AssociateId(id), cc, time.Now())
	shift.PullEvents()
	if err := postgres.NewAssociateRepo(pool).Save(context.Background(), shift); err != nil {
		t.Fatalf("seed associate: %v", err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func idempotencyRowOutcome(t *testing.T, pool *pgxpool.Pool, key string) (storedHash string, statusCode *int, body []byte) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		"SELECT request_hash, status_code, response_body FROM idempotency_keys WHERE key = $1", key,
	).Scan(&storedHash, &statusCode, &body); err != nil {
		t.Fatalf("read idempotency row: %v", err)
	}
	return storedHash, statusCode, body
}

func problemSlug(t *testing.T, body []byte) string {
	t.Helper()
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("decode problem: %v (body: %s)", err, body)
	}
	if idx := strings.LastIndex(problem.Type, "/"); idx != -1 {
		return problem.Type[idx+1:]
	}
	return problem.Type
}

func post(router http.Handler, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(inboundhttp.IdempotencyKeyHeader, key)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------
// POST /shift-plans
// ---------------------------------------------------------------------

const commitBody = `{"buildingId":"B1","shiftId":"S1","lines":[{"pathId":"pack","plannedHeads":5,"plannedRate":30,"plannedHours":40,"installedStations":10}]}`

// (a) fresh key -> 201, plan persisted, outcome recorded verbatim.
func TestIdempotency_Commit_FreshKey_RecordsOutcome(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)

	rec := post(fx.router, "/shift-plans", "commit-fresh-1", commitBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := count(t, pool, "SELECT count(*) FROM shift_plan"); got != 1 {
		t.Fatalf("shift_plan rows = %d, want 1", got)
	}
	_, statusCode, body := idempotencyRowOutcome(t, pool, "commit-fresh-1")
	if statusCode == nil || *statusCode != http.StatusCreated {
		t.Fatalf("stored status_code = %v, want 201", statusCode)
	}
	if string(body) != rec.Body.String() {
		t.Fatal("stored response_body does not match what was returned to the caller")
	}
}

// (b) replay: same key + identical body -> byte-identical response, the real
// handler (its Save and its outbox inserts) never runs a second time.
func TestIdempotency_Commit_Replay_ReturnsIdenticalResponseNoDoubleEffect(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)

	first := post(fx.router, "/shift-plans", "commit-replay-1", commitBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("first call status = %d, want 201 (body: %s)", first.Code, first.Body.String())
	}
	outboxAfterFirst := count(t, pool, "SELECT count(*) FROM outbox_events")
	if outboxAfterFirst == 0 {
		t.Fatal("the commit must have written outbox rows inside the shared transaction")
	}

	second := post(fx.router, "/shift-plans", "commit-replay-1", commitBody)
	if second.Code != first.Code || second.Body.String() != first.Body.String() {
		t.Fatalf("replay differs:\nfirst:  %d %s\nsecond: %d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if second.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("replay Location = %q, want %q", second.Header().Get("Location"), first.Header().Get("Location"))
	}
	if got := fx.publisher.Count(); got != 1 {
		t.Fatalf("publish count after replay = %d, want exactly 1 (no re-execution)", got)
	}
	if got := count(t, pool, "SELECT count(*) FROM outbox_events"); got != outboxAfterFirst {
		t.Fatalf("outbox rows after replay = %d, want %d (no duplicate events)", got, outboxAfterFirst)
	}
}

// (c) same key + different body -> 422.
func TestIdempotency_Commit_SameKeyDifferentBody_Returns422(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)

	if first := post(fx.router, "/shift-plans", "commit-mismatch-1", commitBody); first.Code != http.StatusCreated {
		t.Fatalf("first call status = %d, want 201 (body: %s)", first.Code, first.Body.String())
	}
	second := post(fx.router, "/shift-plans", "commit-mismatch-1", strings.Replace(commitBody, `"S1"`, `"S2"`, 1))
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", second.Code, second.Body.String())
	}
	if slug := problemSlug(t, second.Body.Bytes()); slug != "idempotency-key-reused" {
		t.Fatalf("problem.type slug = %q, want idempotency-key-reused", slug)
	}
	if got := count(t, pool, "SELECT count(*) FROM shift_plan"); got != 1 {
		t.Fatalf("shift_plan rows = %d, want 1 (the mismatched retry must not commit a second plan)", got)
	}
}

// (d) no key header -> 400, the handler never runs.
func TestIdempotency_Commit_NoHeader_Returns400(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)

	rec := post(fx.router, "/shift-plans", "", commitBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if slug := problemSlug(t, rec.Body.Bytes()); slug != "idempotency-key-required" {
		t.Fatalf("problem.type slug = %q, want idempotency-key-required", slug)
	}
	if got := fx.publisher.Count(); got != 0 {
		t.Fatalf("publish count = %d, want 0", got)
	}
}

// (e) concurrency: N real goroutines, same key + body, real HTTP through the
// real router and the real Postgres unique-index lock — exactly one effect.
func TestIdempotency_Commit_Concurrent_ExactlyOneEffect(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)

	const n = 5
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = post(fx.router, "/shift-plans", "commit-concurrent-1", commitBody)
		}(i)
	}
	wg.Wait()

	for i, rec := range results {
		if rec.Code != http.StatusCreated {
			t.Fatalf("goroutine %d status = %d, want 201 (body: %s)", i, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != results[0].Body.String() {
			t.Fatalf("goroutine %d body differs from goroutine 0", i)
		}
	}
	if got := fx.publisher.Count(); got != 1 {
		t.Fatalf("publish count after %d concurrent identical requests = %d, want exactly 1", n, got)
	}
	if got := count(t, pool, "SELECT count(*) FROM shift_plan"); got != 1 {
		t.Fatalf("shift_plan rows = %d, want 1", got)
	}
}

// (f) a business validation error (planned heads over installed stations ->
// 409) is cached and replayed verbatim, not re-decided.
func TestIdempotency_Commit_BusinessErrorResponse_IsCachedAndReplayed(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)
	overBody := strings.Replace(commitBody, `"plannedHeads":5`, `"plannedHeads":11`, 1)

	first := post(fx.router, "/shift-plans", "commit-business-error-1", overBody)
	if first.Code != http.StatusConflict {
		t.Fatalf("first call status = %d, want 409 (body: %s)", first.Code, first.Body.String())
	}
	_, statusCode, _ := idempotencyRowOutcome(t, pool, "commit-business-error-1")
	if statusCode == nil || *statusCode != http.StatusConflict {
		t.Fatalf("stored status_code = %v, want 409 — the business error response must be cached", statusCode)
	}
	second := post(fx.router, "/shift-plans", "commit-business-error-1", overBody)
	if second.Code != first.Code || second.Body.String() != first.Body.String() {
		t.Fatal("replay differs from the cached error response")
	}
	if got := count(t, pool, "SELECT count(*) FROM shift_plan"); got != 0 {
		t.Fatalf("shift_plan rows = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------
// POST /associates/{id}/assignments
// ---------------------------------------------------------------------

const assignBody = `{"pathId":"pack"}`

func countAssignmentEvents(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return count(t, pool, "SELECT count(*) FROM outbox_events WHERE event_type LIKE '%.assignment.Labor%'")
}

func TestIdempotency_Assign_FreshKeyThenReplay_NoDoubleEffect(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)
	seedAssociate(t, pool, "assoc-1", "pack")

	first := post(fx.router, "/associates/assoc-1/assignments", "assign-1", assignBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("first call status = %d, want 201 (body: %s)", first.Code, first.Body.String())
	}
	if got := countAssignmentEvents(t, pool); got != 1 {
		t.Fatalf("assignment outbox rows = %d, want 1", got)
	}

	second := post(fx.router, "/associates/assoc-1/assignments", "assign-1", assignBody)
	if second.Code != first.Code || second.Body.String() != first.Body.String() {
		t.Fatalf("replay differs:\nfirst:  %d %s\nsecond: %d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if got := fx.publisher.Count(); got != 1 {
		t.Fatalf("publish count after replay = %d, want exactly 1", got)
	}
	if got := countAssignmentEvents(t, pool); got != 1 {
		t.Fatalf("assignment outbox rows after replay = %d, want 1 (a blind retry must not re-assign)", got)
	}
}

func TestIdempotency_Assign_SameKeyDifferentBody_Returns422(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)
	seedAssociate(t, pool, "assoc-1", "pack")

	if first := post(fx.router, "/associates/assoc-1/assignments", "assign-mismatch", assignBody); first.Code != http.StatusCreated {
		t.Fatalf("first call status = %d, want 201 (body: %s)", first.Code, first.Body.String())
	}
	second := post(fx.router, "/associates/assoc-1/assignments", "assign-mismatch", `{"pathId":"pack-zone-a"}`)
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", second.Code, second.Body.String())
	}
	if slug := problemSlug(t, second.Body.Bytes()); slug != "idempotency-key-reused" {
		t.Fatalf("problem.type slug = %q, want idempotency-key-reused", slug)
	}
}

func TestIdempotency_Assign_NoHeader_Returns400(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)
	seedAssociate(t, pool, "assoc-1", "pack")

	rec := post(fx.router, "/associates/assoc-1/assignments", "", assignBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if slug := problemSlug(t, rec.Body.Bytes()); slug != "idempotency-key-required" {
		t.Fatalf("problem.type slug = %q, want idempotency-key-required", slug)
	}
	if got := countAssignmentEvents(t, pool); got != 0 {
		t.Fatalf("assignment outbox rows = %d, want 0", got)
	}
}

func TestIdempotency_Assign_Concurrent_ExactlyOneEffect(t *testing.T) {
	pool := idempotencyDB(t)
	fx := newIdempotencyFixture(t, pool)
	seedAssociate(t, pool, "assoc-1", "pack")

	const n = 5
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = post(fx.router, "/associates/assoc-1/assignments", "assign-concurrent", assignBody)
		}(i)
	}
	wg.Wait()

	for i, rec := range results {
		if rec.Code != http.StatusCreated {
			t.Fatalf("goroutine %d status = %d, want 201 (body: %s)", i, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != results[0].Body.String() {
			t.Fatalf("goroutine %d body differs from goroutine 0", i)
		}
	}
	if got := fx.publisher.Count(); got != 1 {
		t.Fatalf("publish count after %d concurrent identical requests = %d, want exactly 1", n, got)
	}
	if got := countAssignmentEvents(t, pool); got != 1 {
		t.Fatalf("assignment outbox rows = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------
// middleware internals only observable with a real database
// ---------------------------------------------------------------------

// The middleware binds ITS transaction into the request context, so the
// wrapped handler (and through it UnitOfWork.Execute) joins it rather than
// opening a second, invisible one.
func TestRequireIdempotencyKey_BindsItsTransactionIntoTheRequestContext(t *testing.T) {
	pool := idempotencyDB(t)
	var bound bool
	h := inboundhttp.RequireIdempotencyKey(pool)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, bound = pgtx.TxFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := post(h, "/x", "bind-1", `{}`)
	if rec.Code != http.StatusNoContent || !bound {
		t.Fatalf("status = %d, tx bound = %v; want 204 and a transaction bound into ctx", rec.Code, bound)
	}
}

// A panic from the wrapped handler rolls the transaction back (no
// idempotency row survives, so a retry re-attempts the real work) and is
// re-raised for the outer Recoverer.
func TestRequireIdempotencyKey_PanicRollsBackAndRePanics(t *testing.T) {
	pool := idempotencyDB(t)
	h := inboundhttp.RequireIdempotencyKey(pool)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	func() {
		defer func() {
			if p := recover(); p != "boom" {
				t.Fatalf("recovered %v, want the handler's panic re-raised", p)
			}
		}()
		post(h, "/x", "panic-1", `{}`)
	}()

	if got := count(t, pool, "SELECT count(*) FROM idempotency_keys WHERE key = 'panic-1'"); got != 0 {
		t.Fatalf("idempotency rows after a panic = %d, want 0 (rolled back, never cached)", got)
	}
}
