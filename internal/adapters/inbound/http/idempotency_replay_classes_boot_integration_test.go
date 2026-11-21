//go:build integration

package http_test

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
)

// bootReplayClassPool boots the ONE disposable, migrated Postgres the
// replay-class reference test shares for its whole run (idempotencyDB, the
// per-test helper, boots a container per call — fine for a handful of
// tests, far too slow for the template's ~30 subtests). No t.Cleanup and no
// explicit terminate: testcontainers' reaper disposes the container when
// the test binary exits, on success or failure alike.
func bootReplayClassPool() (*pgxpool.Pool, error) {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("workforce"),
		tcpostgres.WithUsername("workforce"),
		tcpostgres.WithPassword("workforce"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, err
	}
	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, err
	}
	if err := postgres.Migrate(url, "../../../../migrations"); err != nil {
		return nil, err
	}
	return postgres.NewPool(ctx, url)
}
