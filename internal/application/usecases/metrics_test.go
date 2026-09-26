package usecases

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
)

func TestRejectionReasonMapsEveryGate(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "uncertified", err: assignment.ErrCertificationRequired, want: "uncertified"},
		{name: "on break", err: associate.ErrOnBreak, want: "on_break"},
		{name: "shift ended", err: associate.ErrShiftEnded, want: "shift_ended"},
		{name: "max hours", err: associate.ErrMaxHoursExceeded, want: "max_hours_exceeded"},
		{name: "unknown associate", err: ports.ErrNotFound, want: "associate_not_found"},
		{name: "wrapped sentinel still maps", err: errors.Join(errors.New("save failed"), associate.ErrOnBreak), want: "on_break"},
		{name: "anything else", err: errors.New("boom"), want: "internal_error"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rejectionReason(tc.err); got != tc.want {
				t.Errorf("rejectionReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestRecordLaborAssignmentEmitsOutcomeAttributes reads the counter back out
// of a real SDK MeterProvider, so it verifies the metric name, the
// accepted/rejected split and the reason attribute — not just that the call
// does not panic.
//
// It installs the global MeterProvider exactly once for this test binary:
// the package-level counter is created against the global delegating meter
// at init, and OTel resolves that delegation on the first SetMeterProvider.
func TestRecordLaborAssignmentEmitsOutcomeAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	ctx := context.Background()
	recordLaborAssignment(ctx, "metrics-test-accepted", nil)
	recordLaborAssignment(ctx, "metrics-test-accepted", nil)
	recordLaborAssignment(ctx, "metrics-test-rejected", assignment.ErrCertificationRequired)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	// Path ids are unique to this test so the assertions are unaffected by
	// whatever the AssignLabor use-case tests recorded into the same
	// (process-global) provider.
	got := map[string]int64{}
	for _, p := range laborAssignmentPoints(t, &rm) {
		outcome, _ := p.Attributes.Value(attrOutcome)
		path, _ := p.Attributes.Value(attrPath)
		if !strings.HasPrefix(path.AsString(), "metrics-test-") {
			continue
		}
		key := outcome.AsString() + "/" + path.AsString()
		if outcome.AsString() == outcomeRejected {
			reason, ok := p.Attributes.Value(attrReason)
			if !ok || reason.AsString() != "uncertified" {
				t.Errorf("rejected point reason = %v, want %q", reason.AsString(), "uncertified")
			}
		} else if _, ok := p.Attributes.Value(attrReason); ok {
			t.Error("accepted point carries a reason attribute; it should not")
		}
		got[key] = p.Value
	}

	want := map[string]int64{
		outcomeAccepted + "/metrics-test-accepted": 2,
		outcomeRejected + "/metrics-test-rejected": 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("counter[%s] = %d, want %d", k, got[k], v)
		}
	}
}

// recordingErrorHandler captures whatever otel.Handle was called with.
type recordingErrorHandler struct {
	errs []error
}

func (h *recordingErrorHandler) Handle(err error) { h.errs = append(h.errs, err) }

// TestNewLaborAssignmentsCounterCreationErrorIsHandledNotFatal proves the
// guard branch: when the provider cannot create the instrument (here: a
// view assigns the counter an incompatible aggregation), the error is
// surfaced through otel.Handle and the returned instrument is still usable —
// a metric must never fail a domain operation.
func TestNewLaborAssignmentsCounterCreationErrorIsHandledNotFatal(t *testing.T) {
	// A reader is required for views to be applied at instrument creation;
	// last-value aggregation is incompatible with a counter, so the SDK
	// rejects the creation synchronously.
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewManualReader()),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "workforce.labor_assignments"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationLastValue{}},
		)),
	)
	otel.SetMeterProvider(provider)

	handler := &recordingErrorHandler{}
	otel.SetErrorHandler(handler)

	counter := newLaborAssignmentsCounter()
	if counter == nil {
		t.Fatal("the metric API contract guarantees a usable instrument alongside the error")
	}
	if len(handler.errs) == 0 {
		t.Fatal("expected the instrument-creation error to reach otel.Handle")
	}
	// The returned instrument must be usable, not a panic hazard.
	counter.Add(context.Background(), 1)

	// Leave the package in a working state for any later instrument
	// creation: hand back a clean SDK provider.
	otel.SetMeterProvider(sdkmetric.NewMeterProvider())
}

// TestRecordLaborAssignmentNilCounterIsNoOp proves the nil-meter guard: a
// counter that failed to be created must make recording a silent no-op for
// BOTH outcomes, never a panic.
func TestRecordLaborAssignmentNilCounterIsNoOp(t *testing.T) {
	prev := laborAssignments
	laborAssignments = nil
	defer func() { laborAssignments = prev }()

	recordLaborAssignment(context.Background(), "nil-counter-guard", nil)
	recordLaborAssignment(context.Background(), "nil-counter-guard", assignment.ErrCertificationRequired)
}

// laborAssignmentPoints digs the workforce.labor_assignments sum out of a
// collected ResourceMetrics.
func laborAssignmentPoints(t *testing.T, rm *metricdata.ResourceMetrics) []metricdata.DataPoint[int64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != meterName {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "workforce.labor_assignments" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("workforce.labor_assignments is %T, want metricdata.Sum[int64]", m.Data)
			}
			return sum.DataPoints
		}
	}
	t.Fatal("workforce.labor_assignments was never recorded")
	return nil
}
