package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	segmentio "github.com/segmentio/kafka-go"

	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
)

// --- fakes ------------------------------------------------------------------

type fakeCopy struct {
	rows    map[labourplan.Key]labourplan.Requirement
	saveErr error
	saves   int
}

func (f *fakeCopy) Find(_ context.Context, key labourplan.Key) (labourplan.Requirement, bool, error) {
	r, ok := f.rows[key]
	return r, ok, nil
}

func (f *fakeCopy) Save(_ context.Context, r labourplan.Requirement) error {
	f.saves++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.rows[r.Key] = r
	return nil
}

type fakeClaims struct{ seen map[string]bool }

func (f *fakeClaims) MarkProcessed(_ context.Context, id string) (bool, error) {
	if f.seen[id] {
		return false, nil
	}
	f.seen[id] = true
	return true, nil
}

// fakeUnitOfWork rolls the claims back when fn fails, as the Postgres unit
// of work does: the claim and the upsert share one transaction.
type fakeUnitOfWork struct{ claims *fakeClaims }

func (u fakeUnitOfWork) Execute(ctx context.Context, fn func(ctx context.Context) error) error {
	before := make(map[string]bool, len(u.claims.seen))
	for k, v := range u.claims.seen {
		before[k] = v
	}
	if err := fn(ctx); err != nil {
		u.claims.seen = before
		return err
	}
	return nil
}

type fakeReader struct {
	queue     []segmentio.Message
	committed []segmentio.Message
	commitErr error
	cancel    context.CancelFunc
}

func (r *fakeReader) FetchMessage(ctx context.Context) (segmentio.Message, error) {
	if len(r.queue) == 0 {
		r.cancel()
		return segmentio.Message{}, ctx.Err()
	}
	m := r.queue[0]
	r.queue = r.queue[1:]
	return m, nil
}

func (r *fakeReader) CommitMessages(_ context.Context, msgs ...segmentio.Message) error {
	if r.commitErr != nil {
		return r.commitErr
	}
	r.committed = append(r.committed, msgs...)
	return nil
}

func (r *fakeReader) Close() error { return nil }

type fakeDLQ struct {
	written []segmentio.Message
	err     error
}

func (d *fakeDLQ) WriteMessages(_ context.Context, msgs ...segmentio.Message) error {
	if d.err != nil {
		return d.err
	}
	d.written = append(d.written, msgs...)
	return nil
}

func (d *fakeDLQ) Close() error { return nil }

type harness struct {
	consumer *LabourPlanConsumer
	reader   *fakeReader
	dlq      *fakeDLQ
	copy     *fakeCopy
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{reader: &fakeReader{}, dlq: &fakeDLQ{}, copy: &fakeCopy{rows: map[labourplan.Key]labourplan.Requirement{}}}
	claims := &fakeClaims{seen: map[string]bool{}}
	record := &usecases.RecordLabourRequirement{Requirements: h.copy, Processed: claims, UnitOfWork: fakeUnitOfWork{claims: claims}}
	h.consumer = newLabourPlanConsumer(h.reader, h.dlq, record, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h
}

// fixture loads a testdata event and applies edit to its decoded JSON.
func fixture(t *testing.T, name string, edit func(ev, data map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if edit == nil {
		return raw
	}
	var ev map[string]any
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	data, _ := ev["data"].(map[string]any)
	edit(ev, data)
	out, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func message(value []byte) segmentio.Message {
	return segmentio.Message{Topic: LabourPlanTopic, Key: []byte("WH1/2026-11-18-day/PICK"), Value: value}
}

// --- tests ------------------------------------------------------------------

// Both contract fixtures (with and without the optional shift_start) land in
// the copy with site_id stored as the site code.
func TestLabourPlanConsumer_AppliesBothContractShapes(t *testing.T) {
	cases := []struct {
		file string
		want labourplan.Requirement
	}{
		{"labour-plan-committed.example.json", labourplan.Requirement{
			Key: labourplan.Key{SiteCode: "WH1", ShiftId: "2026-11-18-day", PathId: "PICK"}, PlannedHeads: 6,
			RateUnitsPerHour: 120, Hours: 8, HeadsSource: labourplan.HeadsSourceForecast, Version: 2}},
		{"labour-plan-committed-no-shift-start.example.json", labourplan.Requirement{
			Key: labourplan.Key{SiteCode: "WH1", ShiftId: "2026-11-18-night", PathId: "PACK"}, PlannedHeads: 0,
			RateUnitsPerHour: 45.5, Hours: 7.5, HeadsSource: labourplan.HeadsSourceRequest, Version: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			h := newHarness(t)
			if err := h.consumer.handleFetched(context.Background(), message(fixture(t, tc.file, nil))); err != nil {
				t.Fatal(err)
			}
			if got := h.copy.rows[tc.want.Key]; got != tc.want {
				t.Fatalf("copy = %+v, want %+v", got, tc.want)
			}
			if len(h.reader.committed) != 1 || len(h.dlq.written) != 0 {
				t.Fatalf("committed %d, dead-lettered %d; want 1 and 0", len(h.reader.committed), len(h.dlq.written))
			}
		})
	}
}

func TestLabourPlanConsumer_IgnoresEveryOtherType(t *testing.T) {
	h := newHarness(t)
	other := fixture(t, "labour-plan-committed.example.json", func(ev, _ map[string]any) {
		ev["type"] = "com.warehouse.wes.warehouse-planning.labourplan.LabourPlanDriftDetected"
	})
	if err := h.consumer.handleFetched(context.Background(), message(other)); err != nil {
		t.Fatal(err)
	}
	if h.copy.saves != 0 || len(h.reader.committed) != 1 || len(h.dlq.written) != 0 {
		t.Fatalf("saves %d, committed %d, dead-lettered %d; want 0, 1, 0", h.copy.saves, len(h.reader.committed), len(h.dlq.written))
	}
}

func TestLabourPlanConsumer_RedeliveryIsANoOp(t *testing.T) {
	h := newHarness(t)
	raw := fixture(t, "labour-plan-committed.example.json", nil)
	for range 2 {
		if err := h.consumer.handleFetched(context.Background(), message(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if h.copy.saves != 1 || len(h.reader.committed) != 2 {
		t.Fatalf("saves %d, committed %d; want the redelivery claimed once and both offsets committed", h.copy.saves, len(h.reader.committed))
	}
}

// A malformed message is dead-lettered (raw, with error headers) and
// committed past; nothing reaches the copy.
func TestLabourPlanConsumer_DeadLettersMalformedMessages(t *testing.T) {
	const file = "labour-plan-committed.example.json"
	cases := map[string][]byte{
		"not json":              []byte("{not json"),
		"retired flat envelope": []byte(`{"event_id":"x","event_type":"LabourPlanCommitted","payload":{}}`),
		"missing site":          fixture(t, file, func(_, d map[string]any) { delete(d, "site_id") }),
		"missing shift":         fixture(t, file, func(_, d map[string]any) { d["shift_id"] = "" }),
		"missing path":          fixture(t, file, func(_, d map[string]any) { delete(d, "path_id") }),
		"missing heads":         fixture(t, file, func(_, d map[string]any) { delete(d, "planned_heads") }),
		"negative heads":        fixture(t, file, func(_, d map[string]any) { d["planned_heads"] = -1 }),
		"version zero":          fixture(t, file, func(_, d map[string]any) { d["version"] = 0 }),
		"undecodable data":      fixture(t, file, func(_, d map[string]any) { d["planned_heads"] = "six" }),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if err := h.consumer.handleFetched(context.Background(), message(raw)); err != nil {
				t.Fatal(err)
			}
			if h.copy.saves != 0 || len(h.dlq.written) != 1 || len(h.reader.committed) != 1 {
				t.Fatalf("saves %d, dead-lettered %d, committed %d; want 0, 1, 1", h.copy.saves, len(h.dlq.written), len(h.reader.committed))
			}
			dl := h.dlq.written[0]
			if string(dl.Value) != string(raw) || dl.Topic != "" {
				t.Fatal("the dead letter must carry the raw value and leave the topic to the writer")
			}
		})
	}
}

func TestLabourPlanConsumer_ApplyFailureIsRetriedThenDeadLettered(t *testing.T) {
	h := newHarness(t)
	h.copy.saveErr = errors.New("postgres down")
	if err := h.consumer.handleFetched(context.Background(), message(fixture(t, "labour-plan-committed.example.json", nil))); err != nil {
		t.Fatal(err)
	}
	if h.copy.saves != maxLabourPlanAttempts || len(h.dlq.written) != 1 || len(h.reader.committed) != 1 {
		t.Fatalf("saves %d, dead-lettered %d, committed %d; want %d, 1, 1", h.copy.saves, len(h.dlq.written), len(h.reader.committed), maxLabourPlanAttempts)
	}
}

// Cancellation mid-apply is a shutdown: no dead letter, no commit.
func TestLabourPlanConsumer_CancelledApplyIsNeverDeadLettered(t *testing.T) {
	h := newHarness(t)
	h.copy.saveErr = errors.New("interrupted")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.consumer.handleFetched(ctx, message(fixture(t, "labour-plan-committed.example.json", nil))); !errors.Is(err, context.Canceled) {
		t.Fatalf("handleFetched = %v, want context.Canceled", err)
	}
	if len(h.dlq.written) != 0 || len(h.reader.committed) != 0 {
		t.Fatalf("dead-lettered %d, committed %d; want neither", len(h.dlq.written), len(h.reader.committed))
	}
}

func TestLabourPlanConsumer_RunStopsCleanlyAndSurfacesSinkFailures(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.reader.cancel = cancel
	h.reader.queue = []segmentio.Message{message(fixture(t, "labour-plan-committed.example.json", nil))}
	if err := h.consumer.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want a clean stop", err)
	}
	if len(h.reader.committed) != 1 {
		t.Fatalf("committed %d, want 1", len(h.reader.committed))
	}

	h = newHarness(t)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	h.reader.cancel = cancel
	h.dlq.err = errors.New("broker down")
	h.reader.queue = []segmentio.Message{message([]byte("{not json"))}
	if err := h.consumer.Run(ctx); err == nil {
		t.Fatal("a failed dead-letter write must stop Run")
	}
	if err := h.consumer.Close(); err != nil {
		t.Fatal(err)
	}
}
