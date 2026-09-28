package analyticsstore

import (
	"context"
	"fmt"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxConns is the analytics writer's (cmd/workforce-projector) per-process
// connection ceiling. The projector's Kafka consumer group
// (kafka.AnalyticsConsumerGroup, "workforce-analytics") IS a stable, shared
// group safe to run at N>1 (see the HPA ADR), but its analytics HPA is
// still capped at 2 replicas, not left at api's 4 -- matching
// order-management's reasoning: the publisher partitions by aggregate id
// (AssociateId/PathId, kafka.AnalyticsPublisher.Encode), so ordering is
// only guaranteed per-aggregate, and every write is already idempotent on
// event_id (ConsumedEventsRepo.MarkProcessed), so a wider fan-out buys
// little for what is a lightweight upsert workload. A small, flat pool
// mirrors order-management's analytics writer value.
const MaxConns = 5

// ReportsMaxConns is the analytics reader's (cmd/workforce-reports)
// per-process connection ceiling. reports IS HPA-scalable (stateless REST
// reads, chart's autoscaling.reports block, min 1 / max 3): at the HPA
// ceiling, 3 * 5 = 15 connections against the analytical database. Matches
// order-management's analytics reader value. See the pgxpool/
// statement_timeout ADR for the full connection-budget accounting across
// this service's four processes on the ONE shared Postgres instance.
const ReportsMaxConns = 5

// StatementTimeout bounds the analytics WRITER's (projector) queries.
// Slightly more generous than the OLTP side's 5s: a Kafka consumer
// replaying a backlog after a redeploy issues its upserts in a tight
// loop, and a transient lock wait here should not need to be as tight as
// an interactive OLTP request -- but it must still not be unbounded, or
// one poisoned/oversized batch could wedge a projector instance's only
// connection pool indefinitely. Matches order-management's value.
const StatementTimeout = "10s"

// ReportsStatementTimeout bounds the analytics READER's (reports)
// queries. The labor utilization/staffing report aggregates rows across a
// caller-chosen date range -- wider than the OLTP side's
// always-single-aggregate-by-id shape -- so it gets more headroom than
// StatementTimeout, but still a hard ceiling: a caller-supplied unbounded
// date range must not be able to hold a reports connection forever.
// Matches order-management's value.
const ReportsStatementTimeout = "15s"

// NewPool builds a pgxpool over the analytical database at databaseURL, with
// the OTel pgx tracer installed (mirroring the OLTP postgres.NewPool), plus
// MaxConns and StatementTimeout applied to every connection. It is used by
// the writer (cmd/workforce-projector).
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return newPoolWithLimits(ctx, databaseURL, MaxConns, StatementTimeout, false)
}

// NewReadOnlyPool builds a pgxpool over the analytical database in which every
// connection is pinned to a read-only transaction default
// (default_transaction_read_only=on), with ReportsMaxConns and
// ReportsStatementTimeout applied. The reader process
// (cmd/workforce-reports) uses this so a bug there cannot mutate the read model
// even if the database role itself is not read-only — defence in depth on top
// of the read-only ANALYTICS_DATABASE_URL role (ADR-0010).
func NewReadOnlyPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return newPoolWithLimits(ctx, databaseURL, ReportsMaxConns, ReportsStatementTimeout, true)
}

// newPoolWithLimits is the shared implementation behind NewPool/
// NewReadOnlyPool, parameterised so a test can drive a much shorter
// statementTimeout directly (proving the AfterConnect hook actually
// applies the setting to every new connection, by triggering a real
// cancellation) without waiting out the production value.
func newPoolWithLimits(ctx context.Context, databaseURL string, maxConns int32, statementTimeout string, readOnly bool) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()
	cfg.MaxConns = maxConns
	if readOnly {
		if cfg.ConnConfig.RuntimeParams == nil {
			cfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = '%s'", statementTimeout))
		return err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// RecordPoolStats registers observable gauges for pool's connection counts on
// the global MeterProvider, mirroring the OLTP postgres pool metrics.
func RecordPoolStats(pool *pgxpool.Pool) error {
	return otelpgx.RecordStats(pool)
}
