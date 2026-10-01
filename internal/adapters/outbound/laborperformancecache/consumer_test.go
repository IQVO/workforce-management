package laborperformancecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	ce "github.com/cloudevents/sdk-go/v2/event"
	kafkago "github.com/segmentio/kafka-go"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// fakeReader replays a fixed sequence of messages, then blocks until ctx
// is cancelled -- mirrors a real Kafka reader that has caught up and is
// now waiting for new messages.
type fakeReader struct {
	messages []kafkago.Message
	pos      int
	closed   bool
}

func (r *fakeReader) ReadMessage(ctx context.Context) (kafkago.Message, error) {
	if r.pos < len(r.messages) {
		m := r.messages[r.pos]
		r.pos++
		return m, nil
	}
	<-ctx.Done()
	return kafkago.Message{}, ctx.Err()
}

func (r *fakeReader) Close() error {
	r.closed = true
	return nil
}

func ptr(f float64) *float64 { return &f }

func eventMsg(t *testing.T, partition int, offset int64, eventType string, data any) kafkago.Message {
	t.Helper()
	return cloudEventMsg(t, partition, offset, fmt.Sprintf("p%d-o%d", partition, offset), eventType, data)
}

// cloudEventMsg builds a CloudEvents 1.0 structured-mode message as the
// upstream producer writes it (source/type/subject/time/dataschema set).
func cloudEventMsg(t *testing.T, partition int, offset int64, id, eventType string, data any) kafkago.Message {
	t.Helper()
	e := ce.New(ce.CloudEventsVersionV1)
	e.SetID(id)
	e.SetSource("/warehouse/labor-performance")
	e.SetType(eventType)
	e.SetSubject("subject-1")
	e.SetTime(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	e.SetDataSchema("urn:warehouse:labor-performance:events:TaskPerformanceRecorded:v1")
	if err := e.SetData("application/json", data); err != nil {
		t.Fatalf("set data: %v", err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal cloudevent: %v", err)
	}
	return kafkago.Message{Partition: partition, Offset: offset, Value: raw}
}

func newTestConsumer(reader Reader, target targetOffsets) *Consumer {
	c := &Consumer{
		Logger:     slog.New(slog.DiscardHandler),
		Reader:     reader,
		totals:     make(map[string]runningMean),
		idleTotals: make(map[string]idleShareTotals),
		readyCh:    make(chan struct{}),
		target:     target,
	}
	if len(target) == 0 {
		c.markReady()
	}
	return c
}

func TestConsumer_NoTargetOffsets_IsReadyImmediately(t *testing.T) {
	c := newTestConsumer(&fakeReader{}, targetOffsets{})
	if !c.Ready() {
		t.Fatal("expected a consumer with no readiness target to be ready immediately")
	}
}

func TestConsumer_Run_BecomesReadyAfterCatchingUpSinglePartition(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t1", AssociateId: "a1", TaskType: "PICK", EfficiencyPct: ptr(91.2), ActualSeconds: 40, CompletedAt: "2026-09-05T09:30:00Z"}),
			eventMsg(t, 0, 1, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t2", AssociateId: "a1", TaskType: "PICK", EfficiencyPct: ptr(88.0), ActualSeconds: 60, CompletedAt: "2026-09-05T10:30:00Z"}),
		},
	}
	// target[0] = 2 means "caught up once offset 1 has been processed"
	// (last is exclusive: 2 messages at offsets 0 and 1).
	c := newTestConsumer(reader, targetOffsets{0: 2})

	if c.Ready() {
		t.Fatal("expected not ready before Run starts")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = c.Run(ctx)
		close(done)
	}()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}
	cancel()
	<-done

	got, err := c.MeanActualSeconds(context.Background(), shared.PathId("pick"))
	if err != nil {
		t.Fatalf("expected MeanActualSeconds to succeed, got: %v", err)
	}
	// mean of 40 and 60 is 50.
	if got != 50 {
		t.Fatalf("mean = %v, want 50", got)
	}
}

func TestConsumer_MultiPartition_ReadyOnlyAfterBothCaughtUp(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t1", TaskType: "PICK", ActualSeconds: 40}),
			// Partition 1 not yet caught up (target[1]=1, need offset 0).
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 1, 1: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err == nil {
		t.Fatal("expected WaitReady to time out -- partition 1 never caught up")
	}
}

// TestConsumer_NullEfficiencyPct_StillUpdatesMean is the key contract
// test: a null efficiency_pct means "unscorable", NOT "no duration data"
// -- actual_seconds is always present, so the message must still count
// toward the running mean.
func TestConsumer_NullEfficiencyPct_StillUpdatesMean(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t1", TaskType: "PACK", EfficiencyPct: nil, ActualSeconds: 30}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}

	got, err := c.MeanActualSeconds(context.Background(), shared.PathId("pack"))
	if err != nil {
		t.Fatalf("expected MeanActualSeconds to succeed despite a null efficiency_pct, got: %v", err)
	}
	if got != 30 {
		t.Fatalf("mean = %v, want 30", got)
	}
}

func TestConsumer_UnknownEventType_IsIgnored(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, "SomeFutureEventType", map[string]any{}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout even for an unrecognized event type, got: %v", err)
	}
}

func TestMeanActualSeconds_PathWithNoTaskTypeCounterpart_ReturnsUnavailable(t *testing.T) {
	c := newTestConsumer(&fakeReader{}, targetOffsets{})
	for _, pathId := range []string{"stow", "hazmat", "unknown-path"} {
		_, err := c.MeanActualSeconds(context.Background(), shared.PathId(pathId))
		if !errors.Is(err, ports.ErrMeasuredRateUnavailable) {
			t.Fatalf("path %q: err = %v, want ErrMeasuredRateUnavailable", pathId, err)
		}
	}
}

func TestMeanActualSeconds_NoDataObservedYet_ReturnsUnavailable(t *testing.T) {
	c := newTestConsumer(&fakeReader{}, targetOffsets{})
	_, err := c.MeanActualSeconds(context.Background(), shared.PathId("pick"))
	if !errors.Is(err, ports.ErrMeasuredRateUnavailable) {
		t.Fatalf("err = %v, want ErrMeasuredRateUnavailable", err)
	}
}

func TestConsumer_MultipleTaskTypes_TrackedIndependently(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t1", TaskType: "PICK", ActualSeconds: 40}),
			eventMsg(t, 0, 1, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t2", TaskType: "PACK", ActualSeconds: 20}),
			eventMsg(t, 0, 2, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t3", TaskType: "PICK", ActualSeconds: 60}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 3})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}

	pick, err := c.MeanActualSeconds(context.Background(), shared.PathId("pick"))
	if err != nil {
		t.Fatalf("PICK: %v", err)
	}
	if pick != 50 {
		t.Fatalf("PICK mean = %v, want 50", pick)
	}

	pack, err := c.MeanActualSeconds(context.Background(), shared.PathId("pack"))
	if err != nil {
		t.Fatalf("PACK: %v", err)
	}
	if pack != 20 {
		t.Fatalf("PACK mean = %v, want 20", pack)
	}

	// SLAM has no observed data.
	if _, err := c.MeanActualSeconds(context.Background(), shared.PathId("slam")); !errors.Is(err, ports.ErrMeasuredRateUnavailable) {
		t.Fatalf("SLAM: err = %v, want ErrMeasuredRateUnavailable", err)
	}
}

func TestConsumer_UnattributedTask_EmptyAssociateId_StillUpdatesMean(t *testing.T) {
	// A robot-station task keys on the empty AssociateId (ADR 0013's
	// partition-key rule); this consumer never reads AssociateId at
	// all, so it must fold this message into the mean identically to
	// any attributed task.
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t1", AssociateId: "", TaskType: "SLAM", ActualSeconds: 15}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}

	got, err := c.MeanActualSeconds(context.Background(), shared.PathId("slam"))
	if err != nil {
		t.Fatalf("expected MeanActualSeconds to succeed, got: %v", err)
	}
	if got != 15 {
		t.Fatalf("mean = %v, want 15", got)
	}
}

// TestIdleSharePct_NilIdleSecondsBefore_ContributesActualOnlyAndStaysUnavailable
// is the key idle-share contract test: a nil idle_seconds_before must
// never be coerced into a zero idle contribution -- it feeds the
// denominator (actual_seconds) but leaves the TaskType unavailable until
// at least one message carries a real observation.
func TestIdleSharePct_NilIdleSecondsBefore_ContributesActualOnlyAndStaysUnavailable(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t1", TaskType: "PICK", ActualSeconds: 40, IdleSecondsBefore: nil}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}

	if _, err := c.IdleSharePct(context.Background(), shared.PathId("pick")); !errors.Is(err, ports.ErrIdleShareUnavailable) {
		t.Fatalf("err = %v, want ErrIdleShareUnavailable (nil idle_seconds_before must never fabricate a 0 share)", err)
	}
	// The mean must still reflect the message's actual_seconds.
	mean, err := c.MeanActualSeconds(context.Background(), shared.PathId("pick"))
	if err != nil {
		t.Fatalf("expected MeanActualSeconds to succeed, got: %v", err)
	}
	if mean != 40 {
		t.Fatalf("mean = %v, want 40", mean)
	}
}

// TestIdleSharePct_ComputesRunningShareAcrossMessages covers the core
// sum+count-style ratio: idleSeconds / (idleSeconds + actualSeconds),
// accumulated across multiple messages, including one with a nil
// idle_seconds_before that must contribute to the denominator (via
// actual_seconds) but not the numerator.
func TestIdleSharePct_ComputesRunningShareAcrossMessages(t *testing.T) {
	idle1 := int64(10)
	idle2 := int64(30)
	reader := &fakeReader{
		messages: []kafkago.Message{
			// idle=10, actual=40 -> running idle=10, actual=40
			eventMsg(t, 0, 0, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t1", TaskType: "PACK", ActualSeconds: 40, IdleSecondsBefore: &idle1}),
			// nil idle -> actual-only contribution: running idle=10, actual=60
			eventMsg(t, 0, 1, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t2", TaskType: "PACK", ActualSeconds: 20, IdleSecondsBefore: nil}),
			// idle=30, actual=30 -> running idle=40, actual=90
			eventMsg(t, 0, 2, cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t3", TaskType: "PACK", ActualSeconds: 30, IdleSecondsBefore: &idle2}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 3})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}

	got, err := c.IdleSharePct(context.Background(), shared.PathId("pack"))
	if err != nil {
		t.Fatalf("expected IdleSharePct to succeed, got: %v", err)
	}
	// idle=40, actual=90 -> share = 40/(40+90) = 40/130
	want := 40.0 / 130.0
	if got != want {
		t.Fatalf("idle share = %v, want %v", got, want)
	}
}

// TestIdleSharePct_PathWithNoTaskTypeCounterpart_ReturnsUnavailable
// mirrors TestMeanActualSeconds_PathWithNoTaskTypeCounterpart_ReturnsUnavailable
// for the idle-share port.
func TestIdleSharePct_PathWithNoTaskTypeCounterpart_ReturnsUnavailable(t *testing.T) {
	c := newTestConsumer(&fakeReader{}, targetOffsets{})
	for _, pathId := range []string{"stow", "hazmat", "unknown-path"} {
		_, err := c.IdleSharePct(context.Background(), shared.PathId(pathId))
		if !errors.Is(err, ports.ErrIdleShareUnavailable) {
			t.Fatalf("path %q: err = %v, want ErrIdleShareUnavailable", pathId, err)
		}
	}
}

// TestIdleSharePct_NoDataObservedYet_ReturnsUnavailable mirrors
// TestMeanActualSeconds_NoDataObservedYet_ReturnsUnavailable for the
// idle-share port.
func TestIdleSharePct_NoDataObservedYet_ReturnsUnavailable(t *testing.T) {
	c := newTestConsumer(&fakeReader{}, targetOffsets{})
	if _, err := c.IdleSharePct(context.Background(), shared.PathId("pick")); !errors.Is(err, ports.ErrIdleShareUnavailable) {
		t.Fatalf("err = %v, want ErrIdleShareUnavailable", err)
	}
}

// TestConsumer_LegacyFlatEnvelope_IsRejectedNotParsed proves the retired flat
// envelope is NOT parsed: handle reports ErrNotCloudEvent, no running mean is
// recorded, and Run still skips past it to readiness (ADR-0026).
func TestConsumer_LegacyFlatEnvelope_IsRejectedNotParsed(t *testing.T) {
	legacy := kafkago.Message{Partition: 0, Offset: 0, Value: []byte(`{"event_id":"e1","event_type":"TaskPerformanceRecorded","occurred_at":"2026-09-01T00:00:00Z","source":"labor-performance","data":{"task_id":"t1","task_type":"PICK","actual_seconds":40}}`)}
	c := newTestConsumer(&fakeReader{messages: []kafkago.Message{legacy}}, targetOffsets{0: 1})

	if err := c.handle(legacy); !errors.Is(err, cloudevents.ErrNotCloudEvent) {
		t.Fatalf("handle(legacy) err = %v, want ErrNotCloudEvent", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready after skipping a legacy message, got: %v", err)
	}
	if _, err := c.MeanActualSeconds(context.Background(), shared.PathId("pick")); !errors.Is(err, ports.ErrMeasuredRateUnavailable) {
		t.Fatalf("legacy message must not feed the mean, err = %v", err)
	}
}

// TestConsumer_RedeliveredId_IsFoldedInOnce proves the CloudEvents id dedupe:
// the running mean is a sum, so an at-least-once redelivery of the same
// occurrence must not be counted twice.
func TestConsumer_RedeliveredId_IsFoldedInOnce(t *testing.T) {
	c := newTestConsumer(&fakeReader{}, targetOffsets{})
	first := cloudEventMsg(t, 0, 0, "evt-1", cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t1", TaskType: "PICK", ActualSeconds: 40})
	dup := cloudEventMsg(t, 0, 1, "evt-1", cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t1", TaskType: "PICK", ActualSeconds: 40})
	other := cloudEventMsg(t, 0, 2, "evt-2", cloudevents.TypeTaskPerformanceRecorded, taskPerformanceData{TaskId: "t2", TaskType: "PICK", ActualSeconds: 100})
	for _, m := range []kafkago.Message{first, dup, other} {
		if err := c.handle(m); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	got, err := c.MeanActualSeconds(context.Background(), shared.PathId("pick"))
	if err != nil {
		t.Fatalf("MeanActualSeconds: %v", err)
	}
	if got != 70 {
		t.Fatalf("mean = %v, want 70 (duplicate id must be ignored)", got)
	}
}

// TestConsumer_ShortTypeName_IsIgnored proves dispatch is on the FULL type.
func TestConsumer_ShortTypeName_IsIgnored(t *testing.T) {
	c := newTestConsumer(&fakeReader{}, targetOffsets{})
	if err := c.handle(eventMsg(t, 0, 0, "TaskPerformanceRecorded", taskPerformanceData{TaskId: "t1", TaskType: "PICK", ActualSeconds: 40})); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if _, err := c.MeanActualSeconds(context.Background(), shared.PathId("pick")); !errors.Is(err, ports.ErrMeasuredRateUnavailable) {
		t.Fatalf("short type name must not be dispatched, err = %v", err)
	}
}
