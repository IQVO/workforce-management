//go:build integration

package analyticsstore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/analyticsstore"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/analytics/report"
)

// One analytics Postgres per test binary, not per test: TestMain boots a
// single testcontainers Postgres, applies the analytics migrations once and
// shares it across the package. The fleet rule: a test owns its own
// database — never an external ANALYTICS_DATABASE_URL, never an env-gated
// t.Skip that silently proves nothing on a CI runner.
var (
	mainAnalyticsURL  string
	mainAnalyticsPool *pgxpool.Pool
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("analytics"),
		tcpostgres.WithUsername("workforce"),
		tcpostgres.WithPassword("workforce"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start analytics postgres container: %v\n", err)
		os.Exit(1)
	}

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		_ = testcontainers.TerminateContainer(container)
		os.Exit(1)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrations := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "migrations", "analytics")
	if err := postgres.Migrate(url, migrations); err != nil {
		fmt.Fprintf(os.Stderr, "migrate analytics: %v\n", err)
		_ = testcontainers.TerminateContainer(container)
		os.Exit(1)
	}

	pool, err := analyticsstore.NewPool(ctx, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewPool: %v\n", err)
		_ = testcontainers.TerminateContainer(container)
		os.Exit(1)
	}

	mainAnalyticsURL = url
	mainAnalyticsPool = pool

	code := m.Run()

	pool.Close()
	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "terminate analytics postgres container: %v\n", err)
	}
	os.Exit(code)
}

// analyticsURL returns the shared container's connection string (fresh
// read-only/writer pools are what the tests exercise, so they build their
// own pools per test over the one shared URL).
func analyticsURL(t *testing.T) string {
	t.Helper()
	if mainAnalyticsURL == "" {
		t.Fatal("shared analytics URL not initialised — TestMain did not run")
	}
	return mainAnalyticsURL
}

// analyticsDB returns the shared migrated writer pool, with the analytics
// tables truncated so each test starts from a clean read model.
func analyticsDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if mainAnalyticsPool == nil {
		t.Fatal("shared analytics pool not initialised — TestMain did not run")
	}
	ctx := context.Background()
	for _, table := range []string{"labor_rollup", "analytics_pending_breaks", "analytics_processed_events", "analytics_consumed_events"} {
		if _, err := mainAnalyticsPool.Exec(ctx, "TRUNCATE "+table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return mainAnalyticsPool
}

func TestPostgresProjectionAndReport_RoundTrip(t *testing.T) {
	pool := analyticsDB(t)

	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Hour)
	pathId := "pack-int-" + time.Now().Format("150405.000000000")

	proj := analyticsstore.NewPostgresProjection(pool)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
	}

	// path-scoped: assign twice with the same event id → idempotent.
	apply := func() {
		must(proj.ApplyLaborAssigned(ctx, "int-la", pathId, base))
	}
	apply()
	apply()

	rdr := analyticsstore.NewPostgresReport(pool)
	rep, err := rdr.Query(ctx, report.ReportQuery{
		From:        base.Add(-time.Hour),
		To:          base.Add(time.Hour),
		PathId:      pathId,
		Granularity: report.GranularityHour,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rep.Rows))
	}
	if rep.Rows[0].LaborAssigned != 1 {
		t.Errorf("LaborAssigned = %d, want 1 (idempotent)", rep.Rows[0].LaborAssigned)
	}

	lag, err := rdr.FreshnessLag(ctx)
	if err != nil {
		t.Fatalf("FreshnessLag: %v", err)
	}
	if lag < 0 {
		t.Errorf("lag = %v, want >= 0", lag)
	}
}

// TestReadOnlyPool_RejectsWrites asserts the reader pool is genuinely
// read-only: an attempt to write through it must be rejected by Postgres.
func TestReadOnlyPool_RejectsWrites(t *testing.T) {
	url := analyticsURL(t)

	roPool, err := analyticsstore.NewReadOnlyPool(context.Background(), url)
	if err != nil {
		t.Fatalf("NewReadOnlyPool: %v", err)
	}
	t.Cleanup(roPool.Close)

	ctx := context.Background()
	_, err = roPool.Exec(ctx,
		`INSERT INTO labor_rollup (path_id, hour_bucket) VALUES ($1, $2)`,
		"ro-path", time.Now().UTC().Truncate(time.Hour))
	if err == nil {
		t.Fatal("expected read-only pool to reject INSERT, but it succeeded")
	}

	// The read side still works over the same read-only pool.
	rdr := analyticsstore.NewPostgresReport(roPool)
	if _, err := rdr.FreshnessLag(ctx); err != nil {
		t.Fatalf("FreshnessLag over read-only pool: %v", err)
	}
}

// TestFreshnessLag_EmptyStore covers the NULL path: max(occurred_at) over an
// empty table returns a single NULL row (not zero rows), which must be read as
// a zero lag rather than a scan error.
func TestFreshnessLag_EmptyStore(t *testing.T) {
	pool := analyticsDB(t)

	ctx := context.Background()
	// Ensure the processed-events table is empty so max() yields NULL.
	if _, err := pool.Exec(ctx, `TRUNCATE analytics_processed_events`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	lag, err := analyticsstore.NewPostgresReport(pool).FreshnessLag(ctx)
	if err != nil {
		t.Fatalf("FreshnessLag on empty store: %v", err)
	}
	if lag != 0 {
		t.Fatalf("empty-store lag = %v, want 0", lag)
	}
}
