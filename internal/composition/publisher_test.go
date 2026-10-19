package composition_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/composition"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// lazyPool returns a pgxpool that never dials (pgxpool connects lazily), so
// the wiring can be asserted without a database.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:***@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// There is no log publisher and no direct-to-broker publisher: without a pool
// or without brokers the wiring is a boot error naming the variable.
func TestBuildEventPublisher_RefusesAMissingDependency(t *testing.T) {
	pool := lazyPool(t)
	cases := []struct {
		name string
		pool *pgxpool.Pool
		cfg  composition.PublisherConfig
		want string
	}{
		{"no pool", nil, composition.PublisherConfig{Brokers: []string{"b:9092"}, RunRelay: true}, "DATABASE_URL is required"},
		{"no brokers", pool, composition.PublisherConfig{RunRelay: true}, "KAFKA_BROKERS is required"},
		{"empty brokers", pool, composition.PublisherConfig{Brokers: []string{}}, "KAFKA_BROKERS is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			built, err := composition.BuildEventPublisher(tc.cfg, tc.pool, memory.NewShiftPlanRepo(), quietLogger())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildEventPublisher = %v, want a boot error naming %q", err, tc.want)
			}
			if built.Publisher != nil || built.Relay != nil {
				t.Fatal("a refused wiring must not return a publisher or a relay")
			}
		})
	}
}

// The publisher is ALWAYS the transactional OutboxPublisher (ADR-0016). The
// relay exists only for the process that asked to run it: cmd/workforce does,
// cmd/mcp must not.
func TestBuildEventPublisher_UsesTheOutbox(t *testing.T) {
	pool := lazyPool(t)

	t.Run("relay requested (cmd/workforce)", func(t *testing.T) {
		built, err := composition.BuildEventPublisher(
			composition.PublisherConfig{Brokers: []string{"127.0.0.1:1"}, RunRelay: true, RelayInterval: time.Second},
			pool, postgres.NewShiftPlanRepo(pool), quietLogger())
		if err != nil {
			t.Fatalf("BuildEventPublisher: %v", err)
		}
		defer built.Close()

		if _, ok := built.Publisher.(*postgres.OutboxPublisher); !ok {
			t.Errorf("publisher = %T, want *postgres.OutboxPublisher", built.Publisher)
		}
		if built.Relay == nil {
			t.Error("RunRelay=true must build the outbox relay")
		}
	})

	t.Run("writer only (cmd/mcp)", func(t *testing.T) {
		built, err := composition.BuildEventPublisher(
			composition.PublisherConfig{Brokers: []string{"127.0.0.1:1"}, RunRelay: false},
			pool, postgres.NewShiftPlanRepo(pool), quietLogger())
		if err != nil {
			t.Fatalf("BuildEventPublisher: %v", err)
		}
		defer built.Close()

		if _, ok := built.Publisher.(*postgres.OutboxPublisher); !ok {
			t.Errorf("publisher = %T, want *postgres.OutboxPublisher", built.Publisher)
		}
		if built.Relay != nil {
			t.Error("RunRelay=false must NOT build a relay: only cmd/workforce drains the outbox")
		}
	})
}
