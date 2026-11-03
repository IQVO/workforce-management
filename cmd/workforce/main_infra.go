package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inbound "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/bootretry"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/telemetry"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/composition"
)

// flushTelemetry flushes the telemetry providers within a bound on the way out.
func flushTelemetry(logger *slog.Logger, shutdown func(context.Context) error) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		logger.Warn("telemetry shutdown did not flush cleanly", "error", err)
	}
}

// workforceRepos groups the Postgres repositories, the event publisher,
// and the shared UnitOfWork wired at boot.
type workforceRepos struct {
	associates  *postgres.AssociateRepo
	shiftPlans  *postgres.ShiftPlanRepo
	assignments *postgres.AssignmentRepo
	publisher   ports.EventPublisher
	relay       *postgres.OutboxRelay
	uow         ports.UnitOfWork
}

// wireRepos builds the Postgres repositories, the transactional-outbox event
// publisher with its relay, and the shared UnitOfWork. The returned close func
// releases the publisher adapters; the caller defers it so teardown stays LIFO
// with the pool close. Every publishing use case shares one UnitOfWork so its
// Saves and its Publish (an outbox INSERT) commit together (ADR 0016). The
// relay is not optional: a wiring that yields none is a boot error.
func wireRepos(pool *pgxpool.Pool, brokers []string, logger *slog.Logger) (*workforceRepos, func(), error) {
	shiftPlans := postgres.NewShiftPlanRepo(pool)
	built, err := composition.BuildEventPublisher(publisherConfigFromEnv(brokers), pool, shiftPlans, logger)
	if err != nil {
		return nil, nil, err
	}
	if built.Relay == nil {
		built.Close()
		return nil, nil, errors.New("the outbox relay cannot be built: events would never leave the service")
	}
	return &workforceRepos{
		associates:  postgres.NewAssociateRepo(pool),
		shiftPlans:  shiftPlans,
		assignments: postgres.NewAssignmentRepo(pool),
		publisher:   built.Publisher,
		relay:       built.Relay,
		uow:         postgres.NewUnitOfWork(pool),
	}, built.Close, nil
}

// setupServiceTelemetry wires OTel before any adapter is built and
// returns the resolved service name (the router's metrics label) plus
// the shutdown flush, which the caller defers. A failed final flush is
// logged as a warning, not an error: the usual cause is "no Collector
// listening at OTEL_EXPORTER_OTLP_ENDPOINT", which drops the flush but
// is not a service failure.
func setupServiceTelemetry(ctx context.Context, logger *slog.Logger) (string, func(context.Context) error, error) {
	serviceName := envOrDefault("OTEL_SERVICE_NAME", inbound.DefaultServiceName)
	otlpEndpoint := envOrDefault("OTEL_EXPORTER_OTLP_ENDPOINT", telemetry.DefaultOTLPEndpoint)
	shutdownTelemetry, err := telemetry.Setup(ctx, serviceName, serviceVersion(), otlpEndpoint)
	if err != nil {
		return "", nil, err
	}
	logger.Info("telemetry configured",
		"service_name", serviceName,
		"service_version", serviceVersion(),
		"environment", telemetry.Environment(),
		"otlp_endpoint", otlpEndpoint,
	)
	return serviceName, shutdownTelemetry, nil
}

// openPostgresPool runs the schema migrations, opens the pool, each
// under boot retry: in this fleet EVERY injected pod's FIRST outbound
// TCP dial (here, Postgres) is reset ~10s after the app starts (Istio
// native sidecars). A single attempt turns that known, transient
// condition into CrashLoopBackOff — migrations fail with "read:
// connection reset by peer", the process exits, and the pod never gets
// far enough to serve its own health probe. The retry does not weaken
// the fail-closed rule: once the budget (bootretry.Retries: 5 attempts,
// 1+2+4+8 = 15s of backoff) is exhausted this still refuses to boot.
//
// migrationsDatabaseURL is used ONLY for the golang-migrate step below —
// the pgxpool opened just after it (and used for every subsequent
// request) always uses databaseURL. They are deliberately different
// connection strings in a PgBouncer-fronted environment: golang-migrate's
// postgres driver takes a session-scoped `SELECT pg_advisory_lock($1)` to
// serialize concurrent migration runs across replicas starting at the
// same time, and PgBouncer's transaction-pooling mode (this fleet's
// pool_mode for every OLTP DATABASE_URL, warehouse-infra PR #43) does not
// support session-scoped state — each statement in one logical client
// session can land on a different physical backend connection, so the
// advisory lock never behaves as a real mutex. Losing replicas crash-loop
// with `pq: unnamed prepared statement does not exist` / `pq: canceling
// statement due to statement timeout` until one wins the race. See ADR
// 0025-migrations-direct-postgres-connection.md (and order-management's
// ADR-0029, the reference implementation) for the full incident and fix.
// Callers pass MIGRATIONS_DATABASE_URL when set (warehouse-infra now
// provisions it as a direct, non-pooled DSN alongside DATABASE_URL for
// all 9 OLTP services, per its companion PR #44) or fall back to
// databaseURL itself for any environment that doesn't provision the
// split (local dev, CI integration tests) — byte-identical to this
// function's behavior before this parameter existed in that case.
func openPostgresPool(ctx context.Context, logger *slog.Logger, databaseURL, migrationsDatabaseURL, migrationsPath string) (*pgxpool.Pool, error) {
	if err := bootretry.Retry(ctx, logger, "run migrations", func() error {
		return postgres.Migrate(migrationsDatabaseURL, migrationsPath)
	}); err != nil {
		return nil, err
	}
	var pool *pgxpool.Pool
	if err := bootretry.Retry(ctx, logger, "open postgres pool", func() error {
		var err error
		pool, err = postgres.NewPool(ctx, databaseURL)
		return err
	}); err != nil {
		return nil, err
	}
	return pool, nil
}
