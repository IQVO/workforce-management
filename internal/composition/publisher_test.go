package composition_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/composition"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// lazyPool returns a pgxpool that never dials (pgxpool connects lazily), so
// the wiring can be asserted without a database.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestBuildEventPublisher_DefaultsToLogPublisher(t *testing.T) {
	for _, kind := range []string{"", "log"} {
		built, err := composition.BuildEventPublisher(
			composition.PublisherConfig{Kind: kind, RunRelay: true},
			nil, memory.NewShiftPlanRepo(), quietLogger())
		if err != nil {
			t.Fatalf("kind %q: %v", kind, err)
		}
		if _, ok := built.Publisher.(*events.LogPublisher); !ok {
			t.Errorf("kind %q: publisher = %T, want *events.LogPublisher", kind, built.Publisher)
		}
		if built.Relay != nil {
			t.Errorf("kind %q: relay must be nil for the log publisher", kind)
		}
		built.Close()
	}
}

func TestBuildEventPublisher_UnknownKindIsAnError(t *testing.T) {
	if _, err := composition.BuildEventPublisher(composition.PublisherConfig{Kind: "nats"}, nil, memory.NewShiftPlanRepo(), quietLogger()); err == nil {
		t.Fatal("an unknown EVENT_PUBLISHER must fail boot, not silently log")
	}
}

func TestBuildEventPublisher_KafkaWithoutPoolPublishesDirect(t *testing.T) {
	built, err := composition.BuildEventPublisher(
		composition.PublisherConfig{Kind: composition.KafkaPublisher, Brokers: []string{"127.0.0.1:1"}, RunRelay: true},
		nil, memory.NewShiftPlanRepo(), quietLogger())
	if err != nil {
		t.Fatalf("BuildEventPublisher: %v", err)
	}
	defer built.Close()

	if _, ok := built.Publisher.(*events.MultiPublisher); !ok {
		t.Errorf("publisher = %T, want *events.MultiPublisher (direct mode)", built.Publisher)
	}
	if built.Relay != nil {
		t.Error("direct mode has no outbox, so no relay")
	}
}

// With Postgres, the publisher is ALWAYS the transactional OutboxPublisher
// (ADR-0016). The relay exists only for the process that asked to run it:
// cmd/workforce does, cmd/mcp must not.
func TestBuildEventPublisher_KafkaWithPoolUsesOutbox(t *testing.T) {
	pool := lazyPool(t)

	t.Run("relay requested (cmd/workforce)", func(t *testing.T) {
		built, err := composition.BuildEventPublisher(
			composition.PublisherConfig{Kind: composition.KafkaPublisher, Brokers: []string{"127.0.0.1:1"}, RunRelay: true, RelayInterval: time.Second},
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
			composition.PublisherConfig{Kind: composition.KafkaPublisher, Brokers: []string{"127.0.0.1:1"}, RunRelay: false},
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
