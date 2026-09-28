package kafka

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	segmentio "github.com/segmentio/kafka-go"

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
	for _, msg := range fw.msgs {
		var env envelope
		if err := json.Unmarshal(msg.Value, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.EventType != "ShiftPlanCommitted" {
			t.Errorf("event_type = %q, want ShiftPlanCommitted", env.EventType)
		}
		if env.Source != "workforce-management" {
			t.Errorf("source = %q, want workforce-management", env.Source)
		}
		if env.EventID == "" {
			t.Error("event_id must not be empty")
		}
		if env.OccurredAt != "2026-08-21T22:00:00Z" {
			t.Errorf("occurred_at = %q, want 2026-08-21T22:00:00Z", env.OccurredAt)
		}
		if env.Data.BuildingId != "BLD1" {
			t.Errorf("data.building_id = %q, want BLD1", env.Data.BuildingId)
		}
		if env.Data.ShiftId != "SHIFT1" {
			t.Errorf("data.shift_id = %q, want SHIFT1", env.Data.ShiftId)
		}
		gotPaths[env.Data.PathId] = true
	}
	for _, line := range lines {
		if !gotPaths[string(line.PathId)] {
			t.Errorf("missing message for path %q", line.PathId)
		}
	}
}

func TestPublish_EnvelopeCarriesPathPlanValues(t *testing.T) {
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

	var env envelope
	if err := json.Unmarshal(fw.msgs[0].Value, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	want := envelopeData{
		BuildingId:   "BLD1",
		ShiftId:      "SHIFT1",
		PathId:       "pack",
		PlannedHeads: 3,
		PlannedRate:  50,
		PlannedHours: 24,
	}
	if env.Data != want {
		t.Errorf("data = %+v, want %+v", env.Data, want)
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
