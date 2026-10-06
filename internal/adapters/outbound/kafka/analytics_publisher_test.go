package kafka_test

import (
	"context"
	"testing"
	"time"

	segmentio "github.com/segmentio/kafka-go"

	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// fakeAnalyticsWriter captures the messages handed to WriteMessages so a test
// can assert on the published CloudEvent without a live broker.
type fakeAnalyticsWriter struct {
	msgs []segmentio.Message
}

func (w *fakeAnalyticsWriter) WriteMessages(_ context.Context, msgs ...segmentio.Message) error {
	w.msgs = append(w.msgs, msgs...)
	return nil
}

// goldenAt is the fixed occurred-at every golden case is built with. It is
// deliberately NOT UTC so the golden also proves `time` is normalised to UTC.
var goldenAt = time.Date(2026, 1, 2, 0, 4, 5, 0, time.FixedZone("BRT", -3*3600))

// analyticsGoldenCase pins, for one event type, the exact CloudEvents 1.0
// structured-mode JSON the analytics publisher must write (every required
// attribute, the full type, subject, dataschema and the byte-identical data
// payload) plus the Kafka key.
type analyticsGoldenCase struct {
	name   string
	event  shared.DomainEvent
	key    string
	golden string
}

// analyticsGoldenCases is the per-event-type wire contract for
// warehouse.workforce.analytics (ADR-0026).
var analyticsGoldenCases = []analyticsGoldenCase{
	{
		name:   "AssociateShiftStarted",
		event:  shared.NewAssociateShiftStarted(goldenAt, "a1", nil),
		key:    "a1",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.associate.AssociateShiftStarted","subject":"a1","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:AssociateShiftStarted:v1","time":"2026-01-02T03:04:05Z","data":{"associate_id":"a1"}}`,
	},
	{
		name:   "AssociateShiftEnded",
		event:  shared.NewAssociateShiftEnded(goldenAt, "a2"),
		key:    "a2",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.associate.AssociateShiftEnded","subject":"a2","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:AssociateShiftEnded:v1","time":"2026-01-02T03:04:05Z","data":{"associate_id":"a2"}}`,
	},
	{
		name:   "AssociateBreakStarted",
		event:  shared.NewAssociateBreakStarted(goldenAt, "a3"),
		key:    "a3",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.associate.AssociateBreakStarted","subject":"a3","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:AssociateBreakStarted:v1","time":"2026-01-02T03:04:05Z","data":{"associate_id":"a3"}}`,
	},
	{
		name:   "AssociateBreakEnded",
		event:  shared.NewAssociateBreakEnded(goldenAt, "a4"),
		key:    "a4",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.associate.AssociateBreakEnded","subject":"a4","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:AssociateBreakEnded:v1","time":"2026-01-02T03:04:05Z","data":{"associate_id":"a4"}}`,
	},
	{
		name:   "AssociateCertified",
		event:  shared.NewAssociateCertified(goldenAt, "a5", "hazmat"),
		key:    "a5",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.associate.AssociateCertified","subject":"a5","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:AssociateCertified:v1","time":"2026-01-02T03:04:05Z","data":{"associate_id":"a5","certification":"hazmat"}}`,
	},
	{
		name:   "LaborAssigned",
		event:  shared.NewLaborAssigned(goldenAt, "a6", "pack"),
		key:    "a6",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.assignment.LaborAssigned","subject":"a6","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:LaborAssigned:v1","time":"2026-01-02T03:04:05Z","data":{"associate_id":"a6","path_id":"pack"}}`,
	},
	{
		name:   "LaborReassigned",
		event:  shared.NewLaborReassigned(goldenAt, "a7", "pick", "pack"),
		key:    "a7",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.assignment.LaborReassigned","subject":"a7","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:LaborReassigned:v1","time":"2026-01-02T03:04:05Z","data":{"associate_id":"a7","from_path_id":"pick","to_path_id":"pack"}}`,
	},
	{
		name:   "PathUnderstaffed",
		event:  shared.NewPathUnderstaffed(goldenAt, "pack", 5, 3),
		key:    "pack",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.shiftplan.PathUnderstaffed","subject":"pack","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:PathUnderstaffed:v1","time":"2026-01-02T03:04:05Z","data":{"active_heads":3,"path_id":"pack","planned_heads":5}}`,
	},
	{
		name:   "PathUnderstaffed scoped to a site (additive site_code, ADR 0034)",
		event:  shared.NewPathUnderstaffedAtSite(goldenAt, "pack", 5, 3, "WH1"),
		key:    "pack",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.shiftplan.PathUnderstaffed","subject":"pack","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:PathUnderstaffed:v1","time":"2026-01-02T03:04:05Z","data":{"active_heads":3,"path_id":"pack","planned_heads":5,"site_code":"WH1"}}`,
	},
	{
		name:   "ShiftPlanProposed",
		event:  shared.NewShiftPlanProposed(goldenAt, "b1", "pack", 4, 10.5),
		key:    "pack",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.shiftplan.ShiftPlanProposed","subject":"pack","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:ShiftPlanProposed:v1","time":"2026-01-02T03:04:05Z","data":{"building_id":"b1","path_id":"pack","planned_heads":4,"planned_rate":10.5}}`,
	},
	{
		name:   "ShiftPlanCommitted",
		event:  shared.NewShiftPlanCommitted(goldenAt, "b2", "s2"),
		key:    "b2",
		golden: `{"specversion":"1.0","id":"evt-fixed","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted","subject":"b2/s2","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:ShiftPlanCommitted:v1","time":"2026-01-02T03:04:05Z","data":{"building_id":"b2","shift_id":"s2"}}`,
	},
}

func headerOf(msg segmentio.Message, key string) []string {
	var out []string
	for _, h := range msg.Headers {
		if h.Key == key {
			out = append(out, string(h.Value))
		}
	}
	return out
}

func TestAnalyticsPublisher_GoldenCloudEventPerEventType(t *testing.T) {
	for _, tt := range analyticsGoldenCases {
		t.Run(tt.name, func(t *testing.T) {
			w := &fakeAnalyticsWriter{}
			p := outboundkafka.NewAnalyticsPublisherWithWriter(w, func() string { return "evt-fixed" })

			if err := p.Publish(context.Background(), tt.event); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			if len(w.msgs) != 1 {
				t.Fatalf("expected 1 message, got %d", len(w.msgs))
			}
			msg := w.msgs[0]
			if string(msg.Key) != tt.key {
				t.Errorf("key = %q, want %q", msg.Key, tt.key)
			}
			if got := string(msg.Value); got != tt.golden {
				t.Errorf("value mismatch\n got: %s\nwant: %s", got, tt.golden)
			}
			if ct := headerOf(msg, "content-type"); len(ct) != 1 || ct[0] != "application/cloudevents+json; charset=UTF-8" {
				t.Errorf("content-type header = %v, want exactly one application/cloudevents+json; charset=UTF-8", ct)
			}
		})
	}
}

func TestAnalyticsPublisher_SkipsUnknownEvents(t *testing.T) {
	w := &fakeAnalyticsWriter{}
	p := outboundkafka.NewAnalyticsPublisherWithWriter(w, func() string { return "evt" })

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
