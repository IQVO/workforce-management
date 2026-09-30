package kafka_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	segmentio "github.com/segmentio/kafka-go"

	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// fakeAnalyticsWriter captures the messages handed to WriteMessages so a test
// can assert on the published envelope without a live broker.
type fakeAnalyticsWriter struct {
	msgs []segmentio.Message
}

func (w *fakeAnalyticsWriter) WriteMessages(_ context.Context, msgs ...segmentio.Message) error {
	w.msgs = append(w.msgs, msgs...)
	return nil
}

// analyticsPublishAt is the fixed occurred_at every publisher case is built
// with and asserted against.
var analyticsPublishAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// analyticsPublishCase pins, for one event type, the envelope the analytics
// publisher must write: routing key, event_type, and one representative
// data field from the payload.
type analyticsPublishCase struct {
	name          string
	event         shared.DomainEvent
	wantType      string
	wantKey       string
	wantDataField string
	wantDataValue any
}

// analyticsPublishCases is the per-event-type contract table: every domain
// event this context raises must reach the analytics topic with the right
// key, event_type, and payload shape.
var analyticsPublishCases = []analyticsPublishCase{
	{
		name:          "AssociateShiftStarted",
		event:         shared.NewAssociateShiftStarted(analyticsPublishAt, "a1", nil),
		wantType:      "AssociateShiftStarted",
		wantKey:       "a1",
		wantDataField: "associate_id",
		wantDataValue: "a1",
	},
	{
		name:          "AssociateShiftEnded",
		event:         shared.NewAssociateShiftEnded(analyticsPublishAt, "a2"),
		wantType:      "AssociateShiftEnded",
		wantKey:       "a2",
		wantDataField: "associate_id",
		wantDataValue: "a2",
	},
	{
		name:          "AssociateBreakStarted",
		event:         shared.NewAssociateBreakStarted(analyticsPublishAt, "a3"),
		wantType:      "AssociateBreakStarted",
		wantKey:       "a3",
		wantDataField: "associate_id",
		wantDataValue: "a3",
	},
	{
		name:          "AssociateBreakEnded",
		event:         shared.NewAssociateBreakEnded(analyticsPublishAt, "a4"),
		wantType:      "AssociateBreakEnded",
		wantKey:       "a4",
		wantDataField: "associate_id",
		wantDataValue: "a4",
	},
	{
		name:          "AssociateCertified",
		event:         shared.NewAssociateCertified(analyticsPublishAt, "a5", "hazmat"),
		wantType:      "AssociateCertified",
		wantKey:       "a5",
		wantDataField: "certification",
		wantDataValue: "hazmat",
	},
	{
		name:          "LaborAssigned",
		event:         shared.NewLaborAssigned(analyticsPublishAt, "a6", "pack"),
		wantType:      "LaborAssigned",
		wantKey:       "a6",
		wantDataField: "path_id",
		wantDataValue: "pack",
	},
	{
		name:          "LaborReassigned",
		event:         shared.NewLaborReassigned(analyticsPublishAt, "a7", "pick", "pack"),
		wantType:      "LaborReassigned",
		wantKey:       "a7",
		wantDataField: "to_path_id",
		wantDataValue: "pack",
	},
	{
		name:          "PathUnderstaffed",
		event:         shared.NewPathUnderstaffed(analyticsPublishAt, "pack", 5, 3),
		wantType:      "PathUnderstaffed",
		wantKey:       "pack",
		wantDataField: "active_heads",
		wantDataValue: float64(3),
	},
	{
		name:          "ShiftPlanProposed",
		event:         shared.NewShiftPlanProposed(analyticsPublishAt, "b1", "pack", 4, 10.0),
		wantType:      "ShiftPlanProposed",
		wantKey:       "pack",
		wantDataField: "building_id",
		wantDataValue: "b1",
	},
	{
		name:          "ShiftPlanCommitted",
		event:         shared.NewShiftPlanCommitted(analyticsPublishAt, "b2", "s2"),
		wantType:      "ShiftPlanCommitted",
		wantKey:       "b2",
		wantDataField: "shift_id",
		wantDataValue: "s2",
	},
}

// assertAnalyticsEnvelope publishes one case's event through the analytics
// publisher and asserts the single envelope it wrote: routing key, the
// fixed event_id/source/schema_version/occurred_at wrapper, and one
// representative data field.
func assertAnalyticsEnvelope(t *testing.T, tt analyticsPublishCase) {
	t.Helper()
	w := &fakeAnalyticsWriter{}
	p := outboundkafka.NewAnalyticsPublisher(nil, func() string { return "evt-fixed" })
	p.Writer = w

	if err := p.Publish(context.Background(), tt.event); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(w.msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(w.msgs))
	}
	msg := w.msgs[0]
	if string(msg.Key) != tt.wantKey {
		t.Errorf("key = %q, want %q", string(msg.Key), tt.wantKey)
	}

	var env outboundkafka.AnalyticsEnvelope
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.EventType != tt.wantType {
		t.Errorf("event_type = %q, want %q", env.EventType, tt.wantType)
	}
	if env.EventId != "evt-fixed" {
		t.Errorf("event_id = %q, want evt-fixed", env.EventId)
	}
	if env.Source != "workforce-management" {
		t.Errorf("source = %q, want workforce-management", env.Source)
	}
	if env.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", env.SchemaVersion)
	}
	if !env.OccurredAt.Equal(analyticsPublishAt) {
		t.Errorf("occurred_at = %v, want %v", env.OccurredAt, analyticsPublishAt)
	}

	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if got := data[tt.wantDataField]; got != tt.wantDataValue {
		t.Errorf("data[%q] = %v (%T), want %v (%T)", tt.wantDataField, got, got, tt.wantDataValue, tt.wantDataValue)
	}
}

func TestAnalyticsPublisher_PublishesEachEventType(t *testing.T) {
	for _, tt := range analyticsPublishCases {
		t.Run(tt.name, func(t *testing.T) {
			assertAnalyticsEnvelope(t, tt)
		})
	}
}

func TestAnalyticsPublisher_SkipsUnknownEvents(t *testing.T) {
	w := &fakeAnalyticsWriter{}
	p := outboundkafka.NewAnalyticsPublisher(nil, func() string { return "evt" })
	p.Writer = w

	if err := p.Publish(context.Background(), unknownEvent{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(w.msgs) != 0 {
		t.Fatalf("expected unknown event to be skipped, got %d messages", len(w.msgs))
	}
}

type unknownEvent struct{}

func (unknownEvent) EventName() string     { return "Unknown" }
func (unknownEvent) OccurredAt() time.Time { return time.Time{} }
