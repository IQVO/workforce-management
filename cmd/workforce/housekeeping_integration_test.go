//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
)

// TestStartSweeper_SweepsExpiredIdempotencyKeysAndStopsCleanly proves the
// composition-root wiring (settings -> Sweeper goroutine -> stop func)
// against a real Postgres: an expired idempotency key is removed and the
// stop func returns promptly.
func TestStartSweeper_SweepsExpiredIdempotencyKeysAndStopsCleanly(t *testing.T) {
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
	pool, err := openPostgresPool(ctx, quietLogger(), url, url, "../../migrations")
	if err != nil {
		t.Fatalf("openPostgresPool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `
		INSERT INTO idempotency_keys (key, method, path, request_hash, status_code, created_at)
		VALUES ('stale', 'POST', '/shift-plans', 'h', 201, now() - interval '48 hours'),
		       ('fresh', 'POST', '/shift-plans', 'h', 201, now())`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Setenv("HOUSEKEEPING_INTERVAL", "50ms")
	t.Setenv("IDEMPOTENCY_KEY_TTL", "24h")
	t.Setenv("OUTBOX_RETENTION", "168h")
	cfg := housekeepingSettingsFromEnv(quietLogger())
	if cfg.interval != 50*time.Millisecond || cfg.idempotencyTTL != 24*time.Hour || cfg.outboxRetention != postgres.DefaultOutboxRetention {
		t.Fatalf("settings from env = %+v", cfg)
	}

	stop := startSweeper(pool, cfg, quietLogger())
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sweeper never removed the stale key (rows = %d)", n)
		}
		time.Sleep(50 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("stop did not return")
	}
}
