package kafka

import (
	"context"
	"sync"
	"testing"
	"time"

	segmentio "github.com/segmentio/kafka-go"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// fakeWriter records WriteMessages calls instead of hitting a broker.
type fakeWriter struct {
	mu   sync.Mutex
	msgs []segmentio.Message
}

func (f *fakeWriter) WriteMessages(ctx context.Context, msgs ...segmentio.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, msgs...)
	return nil
}

// assertCommittedLineEvent decodes one ShiftPlanCommitted line message,
// asserts its CloudEvents attributes for the BLD1/SHIFT1 fixture, and returns
// its id and data payload.
func assertCommittedLineEvent(t *testing.T, value []byte) (string, shiftPlanCommittedData) {
	t.Helper()
	e, err := cloudevents.Decode(value)
	if err != nil {
		t.Fatalf("decode cloudevent: %v", err)
	}
	if e.Type() != cloudevents.TypeShiftPlanCommitted {
		t.Errorf("type = %q, want %q", e.Type(), cloudevents.TypeShiftPlanCommitted)
	}
	if e.Source() != "/warehouse/workforce-management" {
		t.Errorf("source = %q, want /warehouse/workforce-management", e.Source())
	}
	if e.ID() == "" {
		t.Error("id must not be empty")
	}
	if e.Subject() != "BLD1/SHIFT1" {
		t.Errorf("subject = %q, want BLD1/SHIFT1", e.Subject())
	}
	if !e.Time().Equal(time.Date(2026, 8, 21, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("time = %v, want 2026-08-21T22:00:00Z", e.Time())
	}
	var data shiftPlanCommittedData
	if err := e.DataAs(&data); err != nil {
		t.Fatalf("data: %v", err)
	}
	if data.BuildingId != "BLD1" || data.ShiftId != "SHIFT1" {
		t.Errorf("data building/shift = %q/%q, want BLD1/SHIFT1", data.BuildingId, data.ShiftId)
	}
	return e.ID(), data
}

func newTestPublisher(t *testing.T, sp *shiftplan.ShiftPlan) (*Publisher, *fakeWriter) {
	t.Helper()
	repo := memory.NewShiftPlanRepo()
	if err := repo.Save(context.Background(), sp); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	fw := &fakeWriter{}
	return NewPublisherWithWriter(fw, repo), fw
}

func mustCommitPlan(t *testing.T, lines []shiftplan.PathPlan, installed map[shared.PathId]int) *shiftplan.ShiftPlan {
	t.Helper()
	sp, err := shiftplan.CommitShiftPlan("BLD1", "SHIFT1", lines, installed, installed, 8.0, time.Date(2026, 8, 21, 22, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("commit shift plan: %v", err)
	}
	return sp
}

func TestPublish_FansOutOneMessagePerPathPlanLine(t *testing.T) {
	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 3, PlannedRate: 50, PlannedHours: 24},
		{PathId: "pick", PlannedHeads: 2, PlannedRate: 40, PlannedHours: 16},
		{PathId: "stow", PlannedHeads: 1, PlannedRate: 30, PlannedHours: 8},
	}
	installed := map[shared.PathId]int{"pack": 5, "pick": 5, "stow": 5}
	sp := mustCommitPlan(t, lines, installed)
	events := sp.PullEvents()

	pub, fw := newTestPublisher(t, sp)
	if err := pub.Publish(context.Background(), events...); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(fw.msgs) != len(lines) {
		t.Fatalf("expected %d messages (one per path plan line), got %d", len(lines), len(fw.msgs))
	}

	gotPaths := make(map[string]bool)
	gotIDs := make(map[string]bool)
	for _, msg := range fw.msgs {
		id, data := assertCommittedLineEvent(t, msg.Value)
		gotIDs[id] = true
		gotPaths[data.PathId] = true
	}
	if len(gotIDs) != len(lines) {
		t.Errorf("every fanned-out line message must carry its own unique id, got %d distinct ids for %d messages", len(gotIDs), len(lines))
	}
	for _, line := range lines {
		if !gotPaths[string(line.PathId)] {
			t.Errorf("missing message for path %q", line.PathId)
		}
	}
}

// TestPublish_GoldenCloudEvent pins the exact wire bytes of one
// ShiftPlanCommitted PathPlan-line message — every CloudEvents attribute, the
// full type string wes-work-planning dispatches on, subject = the ShiftPlan
// aggregate id (== the Kafka key), the integration dataschema and the
// byte-identical data payload — plus the content-type header (ADR-0026).
func TestPublish_GoldenCloudEvent(t *testing.T) {
	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 3, PlannedRate: 50, PlannedHours: 24},
	}
	sp := mustCommitPlan(t, lines, map[shared.PathId]int{"pack": 5})
	events := sp.PullEvents()

	pub, fw := newTestPublisher(t, sp)
	pub.newID = func() string { return "11111111-2222-4333-8444-555555555555" }
	if err := pub.Publish(context.Background(), events...); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(fw.msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(fw.msgs))
	}
	const golden = `{"specversion":"1.0","id":"11111111-2222-4333-8444-555555555555","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted","subject":"BLD1/SHIFT1","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:events:ShiftPlanCommitted:v1","time":"2026-08-21T22:00:00Z","data":{"building_id":"BLD1","shift_id":"SHIFT1","path_id":"pack","planned_heads":3,"planned_rate":50,"planned_hours":24}}`
	if got := string(fw.msgs[0].Value); got != golden {
		t.Errorf("value mismatch\n got: %s\nwant: %s", got, golden)
	}
	if string(fw.msgs[0].Key) != "BLD1/SHIFT1" {
		t.Errorf("key = %q, want BLD1/SHIFT1", fw.msgs[0].Key)
	}
	var ct []string
	for _, h := range fw.msgs[0].Headers {
		if h.Key == "content-type" {
			ct = append(ct, string(h.Value))
		}
	}
	if len(ct) != 1 || ct[0] != "application/cloudevents+json; charset=UTF-8" {
		t.Errorf("content-type header = %v, want exactly one application/cloudevents+json; charset=UTF-8", ct)
	}
}

func TestPublish_DataCarriesPathPlanValues(t *testing.T) {
	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 3, PlannedRate: 50, PlannedHours: 24},
	}
	installed := map[shared.PathId]int{"pack": 5}
	sp := mustCommitPlan(t, lines, installed)
	events := sp.PullEvents()

	pub, fw := newTestPublisher(t, sp)
	if err := pub.Publish(context.Background(), events...); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(fw.msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(fw.msgs))
	}

	e, err := cloudevents.Decode(fw.msgs[0].Value)
	if err != nil {
		t.Fatalf("decode cloudevent: %v", err)
	}
	var got shiftPlanCommittedData
	if err := e.DataAs(&got); err != nil {
		t.Fatalf("data: %v", err)
	}
	want := shiftPlanCommittedData{
		BuildingId:   "BLD1",
		ShiftId:      "SHIFT1",
		PathId:       "pack",
		PlannedHeads: 3,
		PlannedRate:  50,
		PlannedHours: 24,
	}
	if got != want {
		t.Errorf("data = %+v, want %+v", got, want)
	}
}

func TestPublish_IgnoresNonShiftPlanCommittedEvents(t *testing.T) {
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 1, PlannedRate: 10, PlannedHours: 8}}
	installed := map[shared.PathId]int{"pack": 5}
	sp := mustCommitPlan(t, lines, installed)
	sp.PullEvents()

	pub, fw := newTestPublisher(t, sp)
	other := shared.NewAssociateShiftStarted(time.Now(), "A1", nil)
	if err := pub.Publish(context.Background(), other); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(fw.msgs) != 0 {
		t.Fatalf("expected 0 messages for non-ShiftPlanCommitted event, got %d", len(fw.msgs))
	}
}

// TestPublish_AllMessagesForSameShiftPlanCarryIdenticalKey is the regression
// test for the partition-scaleup ordering gap (warehouse-infra PR #42, 1->8
// partitions on warehouse.workforce.events): every message published for the
// SAME ShiftPlan aggregate (building+shift) — both the several PathPlan-line
// messages from one commit, and the messages from a second, later commit
// for that same building/shift — must carry the identical Kafka key, so a
// partitioner routes them to the same partition and per-aggregate ordering
// holds no matter how many partitions the topic has.
func TestPublish_AllMessagesForSameShiftPlanCarryIdenticalKey(t *testing.T) {
	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 3, PlannedRate: 50, PlannedHours: 24},
		{PathId: "pick", PlannedHeads: 2, PlannedRate: 40, PlannedHours: 16},
		{PathId: "stow", PlannedHeads: 1, PlannedRate: 30, PlannedHours: 8},
	}
	installed := map[shared.PathId]int{"pack": 5, "pick": 5, "stow": 5}
	sp := mustCommitPlan(t, lines, installed)
	firstCommitEvents := sp.PullEvents()

	repo := memory.NewShiftPlanRepo()
	if err := repo.Save(context.Background(), sp); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	fw := &fakeWriter{}
	pub := NewPublisherWithWriter(fw, repo)
	if err := pub.Publish(context.Background(), firstCommitEvents...); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(fw.msgs) != len(lines) {
		t.Fatalf("expected %d messages, got %d", len(lines), len(fw.msgs))
	}

	// A second, later commit for the SAME building/shift (e.g. a plan
	// revision) must reuse the exact same key as the first commit's
	// messages, not just agree amongst its own line messages.
	recommitLines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 4, PlannedRate: 50, PlannedHours: 32},
	}
	recommitInstalled := map[shared.PathId]int{"pack": 10}
	sp2, err := shiftplan.CommitShiftPlan("BLD1", "SHIFT1", recommitLines, recommitInstalled, recommitInstalled, 8.0, time.Date(2026, 8, 21, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("recommit shift plan: %v", err)
	}
	secondCommitEvents := sp2.PullEvents()
	if err := repo.Save(context.Background(), sp2); err != nil {
		t.Fatalf("save recommitted plan: %v", err)
	}
	if err := pub.Publish(context.Background(), secondCommitEvents...); err != nil {
		t.Fatalf("publish recommit: %v", err)
	}

	if len(fw.msgs) != len(lines)+len(recommitLines) {
		t.Fatalf("expected %d total messages, got %d", len(lines)+len(recommitLines), len(fw.msgs))
	}

	wantKey := []byte("BLD1/SHIFT1")
	for i, msg := range fw.msgs {
		if string(msg.Key) != string(wantKey) {
			t.Errorf("message %d: key = %q, want %q", i, msg.Key, wantKey)
		}
		if len(msg.Key) == 0 {
			t.Errorf("message %d: key must not be empty", i)
		}
	}
}

func TestPublish_NoEvents_NoWrite(t *testing.T) {
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 1, PlannedRate: 10, PlannedHours: 8}}
	installed := map[shared.PathId]int{"pack": 5}
	sp := mustCommitPlan(t, lines, installed)
	sp.PullEvents()

	pub, fw := newTestPublisher(t, sp)
	if err := pub.Publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(fw.msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(fw.msgs))
	}
}
