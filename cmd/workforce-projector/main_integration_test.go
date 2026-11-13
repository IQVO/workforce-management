//go:build integration

package main

// The boot-level proof of the WRITER composition root (ADR-0010): the real
// run() boots on an EMPTY testcontainers Postgres and applies the analytical
// migrations itself (docs/docs/operations/configuration.md:
// ANALYTICS_MIGRATIONS_PATH, applied on start against
// ANALYTICS_DATABASE_URL), serves GET /healthz on the admin port
// (docs/docs/operations/runbook.md: startup/liveness/readiness all on
// /healthz on admin), projects the CloudEvents the repo's OWN
// AnalyticsPublisher wrote to warehouse.workforce.analytics into the
// labor_rollup fact table — the associate-scoped event under the
// building-wide empty path bucket, the path-scoped event under its path —
// and skips the non-projecting ShiftPlanProposed without recording it
// (internal/adapters/inbound/kafka/analytics_consumer.go). Drain contract:
// SIGTERM -> superviseConsumer returns nil -> clean exit (main.go).

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/analyticsstore"
	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// containerStartTimeout bounds one testcontainers start (repo e2e norm).
const containerStartTimeout = 120 * time.Second

// analyticsMigrations resolves the repo's analytical migrations directory
// the same way internal/adapters/outbound/analyticsstore's own integration
// suite does; the projector is handed it through the documented
// ANALYTICS_MIGRATIONS_PATH knob because the default is relative to the
// deployment's working directory, not the test's.
func analyticsMigrations(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", "analytics")
}

// startEmptyPostgres boots an EMPTY Postgres: no migration is applied by
// the test, because applying the analytical schema is the projector's own
// documented job on start.
func startEmptyPostgres(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTimeout)
	defer cancel()
	c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("analytics_projector_itest"),
		tcpostgres.WithUsername("workforce"),
		tcpostgres.WithPassword("workforce"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres dsn: %v", err)
	}
	return url
}

// startKafka boots the repo's own broker image (how-to-test skill: a
// Kafka-touching integration test starts its own broker, never an env-gate).
func startKafka(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTimeout)
	defer cancel()
	c, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.6.1",
		tckafka.WithClusterID("workforce-projector-itest"))
	if err != nil {
		t.Fatalf("start kafka: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	brokers, err := c.Brokers(ctx)
	if err != nil {
		t.Fatalf("kafka brokers: %v", err)
	}
	return brokers
}

// createTopicAndWaitForLeader creates the one-partition analytics topic and
// waits until it has a leader: conn.CreateTopics returns before the broker
// finished (how-to-test pitfall).
func createTopicAndWaitForLeader(t *testing.T, broker, topic string) {
	t.Helper()
	conn, err := kafkago.Dial("tcp", broker)
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.CreateTopics(kafkago.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}); err != nil {
		t.Fatalf("create topic %s: %v", topic, err)
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		parts, err := conn.ReadPartitions(topic)
		if err == nil && len(parts) == 1 && parts[0].Leader.ID != 0 {
			return
		}
	}
	t.Fatalf("topic %s never got a leader", topic)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// stopWithSIGTERM delivers the binary's real shutdown signal and waits for
// run() to return. The test's own handler is registered FIRST so the
// process survives signals sent before run() reaches its signal.Notify,
// and the signal is retried for determinism (the skill's documented drain
// contract for binaries whose run() takes no context).
func stopWithSIGTERM(t *testing.T, done <-chan error) error {
	t.Helper()
	watch := make(chan os.Signal, 1)
	signal.Notify(watch, syscall.SIGTERM)
	defer signal.Stop(watch)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		select {
		case err := <-done:
			return err
		case <-watch:
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatal("run() did not return after SIGTERM")
	return nil
}

// TestProjectorBootsMigratesProjectsAndStopsCleanly: real run() over an
// EMPTY Postgres + testcontainers Kafka, fed the repo's own encoder events.
func TestProjectorBootsMigratesProjectsAndStopsCleanly(t *testing.T) {
	url := startEmptyPostgres(t)
	brokers := startKafka(t)
	createTopicAndWaitForLeader(t, brokers[0], outboundkafka.AnalyticsTopic)

	// The repo's OWN encoder writes the analytics CloudEvents (ADR-0026):
	// one associate-scoped event (building-wide bucket, path_id ''), one
	// path-scoped event (its path bucket), and one ShiftPlanProposed the
	// consumer must acknowledge without projecting.
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	var seq atomic.Int64
	publisher := outboundkafka.NewAnalyticsPublisher(brokers, func() string {
		return fmt.Sprintf("workforce-projector-itest-%d", seq.Add(1))
	})
	ctx := context.Background()
	err := publisher.Publish(ctx,
		shared.NewAssociateShiftStarted(at, "assoc-proj-1", []shared.Certification{"pack"}),
		shared.NewLaborAssigned(at, "assoc-proj-1", "PACK"),
		shared.NewShiftPlanProposed(at, "WH1", "PACK", 3, 30),
	)
	if err != nil {
		t.Fatalf("publish analytics events: %v", err)
	}

	addr := freeAddr(t)
	t.Setenv("ANALYTICS_DATABASE_URL", url)
	t.Setenv("KAFKA_BROKERS", brokers[0])
	t.Setenv("ANALYTICS_CONSUMER_GROUP", fmt.Sprintf("workforce-projector-itest-%d", time.Now().UnixNano()))
	t.Setenv("ANALYTICS_MIGRATIONS_PATH", analyticsMigrations(t))
	t.Setenv("ADMIN_ADDR", addr)
	t.Setenv("LOG_LEVEL", "error")

	done := make(chan error, 1)
	go func() { done <- run() }()

	waitUntil(t, "/healthz 200 on the admin port", func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	// The documented projection: the shift start lands in the building-wide
	// empty-path bucket, the assignment under PACK; ShiftPlanProposed leaves
	// no trace in the processed set (non-projecting, acknowledged).
	pool, err := analyticsstore.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("open analytics pool: %v", err)
	}
	defer pool.Close()
	waitUntil(t, "AssociateShiftStarted and LaborAssigned projected", func() bool {
		var n int
		err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM labor_rollup WHERE path_id = '' AND hour_bucket = $1 AND shifts_started = 1)
			+ (SELECT count(*) FROM labor_rollup WHERE path_id = 'PACK' AND hour_bucket = $1 AND labor_assigned = 1)`, at).Scan(&n)
		return err == nil && n == 2
	})
	var processed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM analytics_processed_events`).Scan(&processed); err != nil {
		t.Fatalf("count processed events: %v", err)
	}
	if processed != 2 {
		t.Fatalf("analytics_processed_events = %d, want 2: ShiftPlanProposed must not touch the read model or the processed set", processed)
	}

	if err := stopWithSIGTERM(t, done); err != nil {
		t.Fatalf("a SIGTERM'd projector must drain cleanly, got %v", err)
	}
}
