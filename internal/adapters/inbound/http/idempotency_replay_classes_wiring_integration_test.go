//go:build integration

package http_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	inboundhttp "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/pgtx"
)

// newReplayOps is this repo's half of the fleet replay-class reference test
// (idempotency_replay_classes_integration_test.go, rendered verbatim from
// warehouse-harness-template): the six operations over one clean store, as
// closures. This is the only file of the pair that knows the store.
//
// The store is the production design over a real Postgres (testcontainers,
// booted below and shared by the whole run — the template asks for a clean
// store per test and there are ~30 subtests, so newReplayOps TRUNCATEs the
// two tables instead of booting a database each time; the container is
// disposed by testcontainers' reaper when the test process exits):
// RequireIdempotencyKey(pool) begins the request transaction, inserts the
// key row, runs the handler behind a savepoint bound into the context via
// pgtx, and stores/releases the answer per the class mark the production
// writers set. Write goes through that same bound transaction (the unit of
// work the middleware opened); Committed counts from the pool, a different
// connection, so only committed rows are visible; KeyRows counts the
// idempotency table.
func newReplayOps(t *testing.T) replayOps {
	t.Helper()
	pool := replayClassPool(t)
	if _, err := pool.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS replay_probe (tag TEXT NOT NULL)`); err != nil {
		t.Fatalf("create replay_probe: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `TRUNCATE idempotency_keys, replay_probe`); err != nil {
		t.Fatalf("clean the replay-class store: %v", err)
	}
	count := func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	return replayOps{
		Catalogue: func() []problemEntry {
			pts := inboundhttp.ReplayCatalogueForTest()
			out := make([]problemEntry, len(pts))
			for i, p := range pts {
				out[i] = problemEntry{Slug: p.Slug, Status: p.Status, Class: p.Class}
			}
			return out
		},
		Middleware: func(_ *testing.T, next http.Handler) http.Handler {
			return inboundhttp.RequireIdempotencyKey(pool)(next)
		},
		Refuse: func(w http.ResponseWriter, r *http.Request, e problemEntry) {
			inboundhttp.RefuseForTest(w, r, e.Slug)
		},
		Write: func(ctx context.Context, tag string) error {
			tx, ok := pgtx.TxFrom(ctx)
			if !ok {
				return fmt.Errorf("no transaction bound into the request context")
			}
			_, err := tx.Exec(ctx, `INSERT INTO replay_probe (tag) VALUES ($1)`, tag)
			return err
		},
		Committed: func(t *testing.T, tag string) int {
			return count(t, `SELECT count(*) FROM replay_probe WHERE tag = $1`, tag)
		},
		KeyRows: func(t *testing.T, key string) int {
			return count(t, `SELECT count(*) FROM idempotency_keys WHERE key = $1`, key)
		},
	}
}

var (
	replayClassPoolOnce sync.Once
	replayClassPoolVal  *pgxpool.Pool
	replayClassPoolErr  error
)

// replayClassPool boots ONE migrated Postgres for every replay-class test of
// this package (first caller wins; the reaper disposes it at process exit).
func replayClassPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	replayClassPoolOnce.Do(func() {
		pool, err := bootReplayClassPool()
		if err != nil {
			replayClassPoolErr = err
			return
		}
		replayClassPoolVal = pool
	})
	if replayClassPoolErr != nil {
		t.Fatalf("shared replay-class postgres: %v", replayClassPoolErr)
	}
	return replayClassPoolVal
}
