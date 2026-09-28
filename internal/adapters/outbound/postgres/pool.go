// Package postgres provides pgxpool-backed implementations of every
// application port, plus golang-migrate SQL migrations.
package postgres

import (
	"context"
	"fmt"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxConns is the OLTP pool's per-process connection ceiling, shared by
// both cmd/workforce (the api Deployment, HPA-scalable up to
// charts/workforce-management values.yaml's autoscaling.maxReplicas, 4)
// and cmd/mcp (the mcp Deployment, fixed at 1 replica -- see that chart
// value's own doc comment for why it does not get an HPA).
//
// Matches order-management's reference number (ADR-0026 there / this
// repo's own ADR below) rather than re-deriving one: this service shares
// the SAME Postgres instance with up to 9 fleet siblings, each with its
// own logical database, on the unmodified Bitnami default
// max_connections=100 (verified in the reference ADR; not re-verified
// per-service since it's one shared instance, not one per repo). At the
// api Deployment's HPA ceiling of 4 replicas, 4 * 10 = 40 connections
// for this ONE service's OLTP path alone -- the same conservative budget
// order-management chose, deliberately leaving headroom for the other
// fleet services (and this service's own mcp/projector/reports
// processes) sharing the same instance. See the pgxpool/statement_timeout
// ADR for the full connection-budget accounting.
const MaxConns = 10

// StatementTimeout bounds how long a single query may hold a connection
// on the OLTP database before Postgres cancels it. This service's OLTP
// queries are single-aggregate reads/writes (a ShiftPlan, an
// AssociateShift, a LaborAssignment, keyed by id) that normally complete
// in low milliseconds; 5s is generous headroom for lock contention or a
// slow disk without letting one runaway or blocked query hold a pool
// slot -- and therefore a bulkhead slot the HPA's replica math is sizing
// capacity around -- indefinitely. Matches order-management's OLTP
// value. See the pgxpool/statement_timeout ADR.
const StatementTimeout = "5s"

// NewPool opens a connection pool to databaseURL, with MaxConns and
// StatementTimeout applied to every connection.
//
// The pool is wired with otelpgx's query tracer, so every DB round-trip
// becomes a child span of whatever request span is active, carrying the
// normalized SQL statement (otelpgx sanitizes it — literal values are not
// recorded, so spans never leak row data).
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return NewPoolWithLimits(ctx, databaseURL, MaxConns, StatementTimeout)
}

// NewPoolWithLimits is NewPool's shared implementation, taking maxConns
// and statementTimeout explicitly so an integration test can drive a
// much shorter timeout directly -- proving the AfterConnect hook really
// applies the setting to every new connection, by triggering an actual
// cancellation -- without waiting out the real production value.
// Production callers should use NewPool.
func NewPoolWithLimits(ctx context.Context, databaseURL string, maxConns int32, statementTimeout string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()
	cfg.MaxConns = maxConns
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = '%s'", statementTimeout))
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}
