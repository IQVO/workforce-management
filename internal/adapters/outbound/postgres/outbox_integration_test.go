//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// outboxDB boots a throwaway Postgres (testcontainers — the test owns its
// own database, never an external DATABASE_URL) and runs the OLTP
// migrations, including 000002_outbox.
func outboxDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	_, thisFile, _, _ := runtime.Caller(0)
	migrations := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "migrations")
	if err := postgres.Migrate(url, migrations); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	pool, err := postgres.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type fixedCapacity map[shared.PathId]int

func (c fixedCapacity) InstalledCapacity(_ context.Context, id shared.PathId) (int, error) {
	return c[id], nil
}

// recordingSink records what the relay sends and can fail on one event type.
type recordingSink struct {
	sent    []outboundkafka.Encoded
	failOn  string // EventType to fail on, "" for never
	failErr error
}

func (s *recordingSink) Send(_ context.Context, msgs ...outboundkafka.Encoded) error {
	for _, m := range msgs {
		if s.failOn != "" && m.EventType == s.failOn {
			return s.failErr
		}
		s.sent = append(s.sent, m)
	}
	return nil
}

// failingEncoder stands in for a broken encoder so the outbox insert fails
// while the aggregate Save already succeeded inside the same transaction.
type failingEncoder struct{ err error }

func (f failingEncoder) Encode(context.Context, ...shared.DomainEvent) ([]outboundkafka.Encoded, error) {
	return nil, f.err
}

func countOutbox(t *testing.T, pool *pgxpool.Pool, where string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM outbox_events WHERE "+where).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// deps wires the production adapters over pool exactly as cmd/workforce
// does in outbox mode: both Kafka publishers act as Encoders feeding one
// OutboxPublisher, and every use case shares one UnitOfWork.
type deps struct {
	associates  ports.AssociateRepo
	shiftPlans  ports.ShiftPlanRepo
	assignments ports.AssignmentRepo
	pub         ports.EventPublisher
	uow         ports.UnitOfWork
}

func newDeps(pool *pgxpool.Pool) deps {
	shiftPlans := postgres.NewShiftPlanRepo(pool)
	integration := outboundkafka.NewPublisherWithWriter(nil, shiftPlans)
	analytics := outboundkafka.NewAnalyticsPublisherWithWriter(nil, outboundkafka.NewEventID)
	return deps{
		associates:  postgres.NewAssociateRepo(pool),
		shiftPlans:  shiftPlans,
		assignments: postgres.NewAssignmentRepo(pool),
		pub:         postgres.NewOutboxPublisher(pool, integration, analytics),
		uow:         postgres.NewUnitOfWork(pool),
	}
}

func withSpan(t *testing.T) context.Context {
	t.Helper()
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "request")
	t.Cleanup(func() { span.End() })
	return ctx
}

func TestOutbox_CommitShiftPlan_CommitsAggregateAndBothTopicsTogether(t *testing.T) {
	pool := outboxDB(t)
	ctx := withSpan(t)
	d := newDeps(pool)
	uc := &usecases.CommitShiftPlan{
		ShiftPlans:        d.shiftPlans,
		Events:            d.pub,
		Clock:             fixedClock{t: time.Now().UTC()},
		InstalledCapacity: fixedCapacity{"pack": 10, "pick": 10},
		MaxHoursPerShift:  8,
		UnitOfWork:        d.uow,
	}

	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 3, PlannedRate: 50, PlannedHours: 24},
		{PathId: "pick", PlannedHeads: 2, PlannedRate: 40, PlannedHours: 16},
	}
	if _, err := uc.Execute(ctx, "BLD1", "SHIFT1", lines, map[shared.PathId]int{"pack": 10, "pick": 10}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Aggregate present (the ShiftPlanRepo's own multi-statement Save joined
	// the unit of work rather than opening its own transaction).
	sp, err := d.shiftPlans.FindByBuildingAndShift(ctx, "BLD1", "SHIFT1")
	if err != nil || len(sp.Lines()) != 2 {
		t.Fatalf("expected the plan persisted with 2 lines, got %v err=%v", sp, err)
	}
	// One integration row per PathPlan line — proof that the integration
	// Encoder's repo read happened INSIDE the transaction and saw the
	// just-saved plan.
	if got := countOutbox(t, pool, "published_at IS NULL AND topic = '"+outboundkafka.Topic+"' AND event_type = '"+cloudevents.TypeShiftPlanCommitted+"'"); got != 2 {
		t.Fatalf("expected 2 unpublished integration rows (one per line), got %d", got)
	}
	// Exactly one analytics row for the same event.
	if got := countOutbox(t, pool, "published_at IS NULL AND topic = '"+outboundkafka.AnalyticsTopic+"' AND event_type = '"+cloudevents.TypeShiftPlanCommitted+"'"); got != 1 {
		t.Fatalf("expected 1 unpublished analytics row, got %d", got)
	}
	// Trace headers and the CloudEvents content-type header persisted as JSON.
	var headers []byte
	if err := pool.QueryRow(ctx, "SELECT headers FROM outbox_events ORDER BY id LIMIT 1").Scan(&headers); err != nil {
		t.Fatalf("read headers: %v", err)
	}
	var hs []map[string]string
	if err := json.Unmarshal(headers, &hs); err != nil {
		t.Fatalf("decode headers %s: %v", headers, err)
	}
	got := map[string]string{}
	for _, h := range hs {
		got[h["key"]] = h["value"]
	}
	if got["traceparent"] == "" || got["content-type"] != cloudevents.MediaType {
		t.Fatalf("expected persisted traceparent and content-type headers, got %s", headers)
	}
	// Every persisted value is a valid CloudEvent, and the fanned-out
	// integration rows each carry their own unique id.
	rows, err := pool.Query(ctx, "SELECT topic, value FROM outbox_events ORDER BY id")
	if err != nil {
		t.Fatalf("read values: %v", err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var topic string
		var value []byte
		if err := rows.Scan(&topic, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		e, err := cloudevents.Decode(value)
		if err != nil {
			t.Fatalf("persisted %s value is not a CloudEvent: %v", topic, err)
		}
		if ids[e.ID()] {
			t.Fatalf("duplicate CloudEvents id %q across outbox rows", e.ID())
		}
		ids[e.ID()] = true
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 distinct ids (2 integration lines + 1 analytics), got %d", len(ids))
	}
}

func TestOutbox_AssignLabor_CommitsAssociateAssignmentAndEventTogether(t *testing.T) {
	pool := outboxDB(t)
	ctx := context.Background()
	d := newDeps(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := &usecases.StartAssociateShift{Associates: d.associates, Events: d.pub, Clock: fixedClock{t: now}, UnitOfWork: d.uow}
	assign := &usecases.AssignLabor{Associates: d.associates, Assignments: d.assignments, Events: d.pub, Clock: fixedClock{t: now}, MaxHoursPerShift: 8, UnitOfWork: d.uow}

	if _, err := start.Execute(ctx, "A1", []shared.Certification{"pack", "stow"}); err != nil {
		t.Fatalf("start shift: %v", err)
	}
	if _, err := assign.Execute(ctx, "A1", "pack"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	assign.Clock = fixedClock{t: now.Add(2 * time.Hour)}
	if _, err := assign.Execute(ctx, "A1", "stow"); err != nil {
		t.Fatalf("reassign: %v", err)
	}

	la, err := d.assignments.FindByAssociateID(ctx, "A1")
	if err != nil {
		t.Fatalf("find assignment: %v", err)
	}
	if p, active := la.ActivePathId(); !active || p != "stow" || len(la.History()) != 1 {
		t.Fatalf("expected active stow with one closed interval, got active=%v path=%s history=%d", active, p, len(la.History()))
	}
	a, err := d.associates.FindByID(ctx, "A1")
	if err != nil || a.HoursLogged() != 2 {
		t.Fatalf("expected 2 hours logged on the associate, got %v err=%v", a, err)
	}
	// Analytics rows: AssociateShiftStarted, LaborAssigned, LaborReassigned.
	// No integration rows: the integration contract only carries
	// ShiftPlanCommitted.
	if got := countOutbox(t, pool, "topic = '"+outboundkafka.AnalyticsTopic+"'"); got != 3 {
		t.Fatalf("expected 3 analytics rows, got %d", got)
	}
	if got := countOutbox(t, pool, "topic = '"+outboundkafka.Topic+"'"); got != 0 {
		t.Fatalf("expected no integration rows for associate events, got %d", got)
	}
}

// The whole point of the outbox: if the event cannot be enqueued the
// aggregate change must not survive either. A failing Encoder makes the
// outbox insert fail AFTER the ShiftPlanRepo already wrote its rows inside
// the same transaction.
func TestOutbox_PublishFailure_RollsBackAggregate(t *testing.T) {
	pool := outboxDB(t)
	ctx := context.Background()
	shiftPlans := postgres.NewShiftPlanRepo(pool)
	uc := &usecases.CommitShiftPlan{
		ShiftPlans:        shiftPlans,
		Events:            postgres.NewOutboxPublisher(pool, failingEncoder{err: errors.New("encoder exploded")}),
		Clock:             fixedClock{t: time.Now().UTC()},
		InstalledCapacity: fixedCapacity{"pack": 10},
		MaxHoursPerShift:  8,
		UnitOfWork:        postgres.NewUnitOfWork(pool),
	}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 3, PlannedRate: 50, PlannedHours: 24}}
	if _, err := uc.Execute(ctx, "BLD2", "SHIFT2", lines, map[shared.PathId]int{"pack": 10}); err == nil {
		t.Fatal("expected the failing encoder to fail the commit")
	}
	if _, err := shiftPlans.FindByBuildingAndShift(ctx, "BLD2", "SHIFT2"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("aggregate row survived a failed publish: the unit of work did not roll back (err=%v)", err)
	}
	if got := countOutbox(t, pool, "true"); got != 0 {
		t.Fatalf("expected no outbox rows, got %d", got)
	}

	// Same guarantee for the single-statement AssociateRepo path, and for
	// a genuine INSERT failure (over-long value is not applicable to BYTEA;
	// use a NULL-violating topic instead via an encoder that omits it).
	assoc := postgres.NewAssociateRepo(pool)
	start := &usecases.StartAssociateShift{
		Associates: assoc,
		Events:     postgres.NewOutboxPublisher(pool, nullValueEncoder{}),
		Clock:      fixedClock{t: time.Now().UTC()},
		UnitOfWork: postgres.NewUnitOfWork(pool),
	}
	if _, err := start.Execute(ctx, "GHOST", nil); err == nil {
		t.Fatal("expected the NOT NULL violation to fail the insert")
	}
	if _, err := assoc.FindByID(ctx, "GHOST"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("associate row survived a failed outbox insert (err=%v)", err)
	}
}

// nullValueEncoder yields a message with a nil Value, violating
// outbox_events.value NOT NULL at INSERT time.
type nullValueEncoder struct{}

func (nullValueEncoder) Encode(_ context.Context, evts ...shared.DomainEvent) ([]outboundkafka.Encoded, error) {
	out := make([]outboundkafka.Encoded, len(evts))
	for i, e := range evts {
		out[i] = outboundkafka.Encoded{Topic: "t", EventType: e.EventName()}
	}
	return out, nil
}

func TestOutboxRelay_PublishesInIdOrderAcrossTopicsAndMarksRows(t *testing.T) {
	pool := outboxDB(t)
	ctx := context.Background()
	d := newDeps(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := &usecases.StartAssociateShift{Associates: d.associates, Events: d.pub, Clock: fixedClock{t: now}, UnitOfWork: d.uow}
	commit := &usecases.CommitShiftPlan{ShiftPlans: d.shiftPlans, Events: d.pub, Clock: fixedClock{t: now}, InstalledCapacity: fixedCapacity{"pack": 10}, MaxHoursPerShift: 8, UnitOfWork: d.uow}
	certify := &usecases.CertifyAssociate{Associates: d.associates, Events: d.pub, Clock: fixedClock{t: now.Add(time.Second)}, UnitOfWork: d.uow}

	if _, err := start.Execute(ctx, "A1", nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := commit.Execute(ctx, "BLD1", "S1", []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 1, PlannedRate: 10, PlannedHours: 8}}, map[shared.PathId]int{"pack": 10}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := certify.Execute(ctx, "A1", "hazmat"); err != nil {
		t.Fatalf("certify: %v", err)
	}

	sink := &recordingSink{}
	relay := postgres.NewOutboxRelay(pool, sink, slog.Default())
	n, err := relay.RelayOnce(ctx)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	// AssociateShiftStarted(analytics), ShiftPlanCommitted(integration),
	// ShiftPlanCommitted(analytics), AssociateCertified(analytics).
	want := []struct{ topic, typ string }{
		{outboundkafka.AnalyticsTopic, cloudevents.TypeAssociateShiftStarted},
		{outboundkafka.Topic, cloudevents.TypeShiftPlanCommitted},
		{outboundkafka.AnalyticsTopic, cloudevents.TypeShiftPlanCommitted},
		{outboundkafka.AnalyticsTopic, cloudevents.TypeAssociateCertified},
	}
	if n != len(want) || len(sink.sent) != len(want) {
		t.Fatalf("expected %d published, got n=%d sent=%d", len(want), n, len(sink.sent))
	}
	for i, w := range want {
		if sink.sent[i].Topic != w.topic || sink.sent[i].EventType != w.typ {
			t.Fatalf("message %d: want %s on %s, got %s on %s", i, w.typ, w.topic, sink.sent[i].EventType, sink.sent[i].Topic)
		}
	}
	if string(sink.sent[0].Key) != "A1" {
		t.Fatalf("analytics key must be the aggregate id, got %q", sink.sent[0].Key)
	}
	// The relay republishes the exact persisted bytes: the CloudEvents id was
	// minted once at Encode time, so the relayed id is the stored id.
	var storedValue []byte
	if err := pool.QueryRow(ctx, "SELECT value FROM outbox_events ORDER BY id LIMIT 1").Scan(&storedValue); err != nil {
		t.Fatalf("read stored value: %v", err)
	}
	if string(storedValue) != string(sink.sent[0].Value) {
		t.Fatalf("relayed value differs from the persisted outbox value")
	}
	if e, err := cloudevents.Decode(sink.sent[0].Value); err != nil || e.ID() == "" {
		t.Fatalf("relayed message is not a CloudEvent with an id: %v", err)
	}
	if got := countOutbox(t, pool, "published_at IS NULL"); got != 0 {
		t.Fatalf("expected every row marked published, %d still pending", got)
	}
	if got := countOutbox(t, pool, "attempts = 1 AND last_error IS NULL"); got != len(want) {
		t.Fatalf("expected attempts=1/last_error NULL on every row, got %d", got)
	}
	// A second pass finds nothing and republishes nothing.
	n, err = relay.RelayOnce(ctx)
	if err != nil || n != 0 || len(sink.sent) != len(want) {
		t.Fatalf("second pass should be a no-op, got n=%d err=%v sent=%d", n, err, len(sink.sent))
	}
}

func TestOutboxRelay_SinkFailure_StopsAtFailedRowAndRecoversInOrder(t *testing.T) {
	pool := outboxDB(t)
	ctx := context.Background()
	d := newDeps(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := &usecases.StartAssociateShift{Associates: d.associates, Events: d.pub, Clock: fixedClock{t: now}, UnitOfWork: d.uow}
	certify := &usecases.CertifyAssociate{Associates: d.associates, Events: d.pub, Clock: fixedClock{t: now}, UnitOfWork: d.uow}
	startBreak := &usecases.StartBreak{Associates: d.associates, Events: d.pub, Clock: fixedClock{t: now}, UnitOfWork: d.uow}

	if _, err := start.Execute(ctx, "A1", nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := certify.Execute(ctx, "A1", "pack"); err != nil {
		t.Fatalf("certify: %v", err)
	}
	if err := startBreak.Execute(ctx, "A1"); err != nil {
		t.Fatalf("break: %v", err)
	}

	sink := &recordingSink{failOn: cloudevents.TypeAssociateCertified, failErr: errors.New("broker down")}
	relay := postgres.NewOutboxRelay(pool, sink, slog.Default())
	n, err := relay.RelayOnce(ctx)
	if err == nil {
		t.Fatal("expected the failing row to surface an error")
	}
	if n != 1 || len(sink.sent) != 1 || sink.sent[0].EventType != cloudevents.TypeAssociateShiftStarted {
		t.Fatalf("expected only AssociateShiftStarted published before the failure, got n=%d sent=%v", n, sink.sent)
	}
	if got := countOutbox(t, pool, "published_at IS NULL"); got != 2 {
		t.Fatalf("expected AssociateCertified and AssociateBreakStarted still pending (ordering preserved), got %d pending", got)
	}
	var attempts int
	var lastErr string
	if err := pool.QueryRow(ctx, "SELECT attempts, coalesce(last_error,'') FROM outbox_events WHERE event_type = $1", cloudevents.TypeAssociateCertified).Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("read failed row: %v", err)
	}
	if attempts != 1 || lastErr == "" {
		t.Fatalf("expected the failed row to record the attempt, got attempts=%d last_error=%q", attempts, lastErr)
	}
	if got := countOutbox(t, pool, "event_type = '"+cloudevents.TypeAssociateBreakStarted+"' AND attempts = 0"); got != 1 {
		t.Fatal("the row behind the failure must remain untouched")
	}

	// Broker recovers: the next pass drains the rest, in order.
	sink.failOn = ""
	n, err = relay.RelayOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("recovery pass: n=%d err=%v", n, err)
	}
	if sink.sent[1].EventType != cloudevents.TypeAssociateCertified || sink.sent[2].EventType != cloudevents.TypeAssociateBreakStarted {
		t.Fatalf("expected AssociateCertified then AssociateBreakStarted after recovery, got %v", sink.sent)
	}
	if got := countOutbox(t, pool, "published_at IS NULL"); got != 0 {
		t.Fatalf("expected outbox drained, %d pending", got)
	}
	if got := countOutbox(t, pool, "event_type = '"+cloudevents.TypeAssociateCertified+"' AND attempts = 2 AND last_error IS NULL"); got != 1 {
		t.Fatal("expected the recovered row to clear last_error and count both attempts")
	}
}

func TestOutboxRelay_RunDrainsUntilCancelled(t *testing.T) {
	pool := outboxDB(t)
	ctx := context.Background()
	d := newDeps(pool)
	start := &usecases.StartAssociateShift{Associates: d.associates, Events: d.pub, Clock: fixedClock{t: time.Now().UTC()}, UnitOfWork: d.uow}
	for _, id := range []shared.AssociateId{"A1", "A2", "A3"} {
		if _, err := start.Execute(ctx, id, nil); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
	}
	sink := &recordingSink{}
	// Batch size 1 forces the "full batch => immediate next pass" branch.
	relay := postgres.NewOutboxRelay(pool, sink, slog.Default(), postgres.WithBatchSize(1), postgres.WithInterval(20*time.Millisecond))
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := relay.Run(runCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected Run to return the ctx error, got %v", err)
	}
	if len(sink.sent) != 3 || countOutbox(t, pool, "published_at IS NULL") != 0 {
		t.Fatalf("expected Run to drain all 3 rows, sent=%d", len(sink.sent))
	}
}
