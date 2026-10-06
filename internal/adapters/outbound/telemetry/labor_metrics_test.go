package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/claudioed/workforce-management/internal/application/ports"
)

// laborAssignmentPoints digs the workforce.labor_assignments sum out of a
// collected ResourceMetrics.
func laborAssignmentPoints(t *testing.T, rm *metricdata.ResourceMetrics) []metricdata.DataPoint[int64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name != "workforce.labor_assignments" {
				continue
			}
			sum, ok := met.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("workforce.labor_assignments is %T, want metricdata.Sum[int64]", met.Data)
			}
			return sum.DataPoints
		}
	}
	t.Fatal("workforce.labor_assignments was never recorded")
	return nil
}

// recordTwoAcceptedOneRejected drives the adapter through both outcomes on
// path ids unique to these tests, so nothing else recorded into a shared
// provider can interfere.
func recordTwoAcceptedOneRejected(t *testing.T, m *LaborMetrics) {
	t.Helper()
	ctx := context.Background()
	m.AssignmentAccepted(ctx, "metrics-test-accepted")
	m.AssignmentAccepted(ctx, "metrics-test-accepted")
	m.AssignmentRejected(ctx, "metrics-test-rejected", "uncertified")
}

// TestLaborMetricsEmitsOutcomeSplitAndCounts reads the counter back out of
// a real SDK MeterProvider and verifies the accepted/rejected split and
// per-series counts.
func TestLaborMetricsEmitsOutcomeSplitAndCounts(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	m, err := NewLaborMetrics()
	if err != nil || m == nil {
		t.Fatalf("NewLaborMetrics = %v, %v; want non-nil, nil", m, err)
	}
	recordTwoAcceptedOneRejected(t, m)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	points := laborAssignmentPoints(t, &rm)
	if len(points) != 2 {
		t.Fatalf("collected %d points, want 2 (accepted and rejected)", len(points))
	}

	got := map[string]int64{}
	for _, p := range points {
		outcome, _ := p.Attributes.Value(laborOutcomeKey)
		path, _ := p.Attributes.Value(laborPathKey)
		got[outcome.AsString()+"/"+path.AsString()] = p.Value
	}
	want := map[string]int64{
		laborOutcomeAccepted + "/metrics-test-accepted": 2,
		laborOutcomeRejected + "/metrics-test-rejected": 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("counter[%s] = %d, want %d", k, got[k], v)
		}
	}
}

// TestLaborMetricsAttributeKeysMatchFleetConvention pins the Tier-2
// attribute-key contract (ADR-0015): outcome and reason — plain, with no
// service-specific workforce.assignment.* prefix — and reason present only
// on rejected points.
func TestLaborMetricsAttributeKeysMatchFleetConvention(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	m, err := NewLaborMetrics()
	if err != nil || m == nil {
		t.Fatalf("NewLaborMetrics = %v, %v; want non-nil, nil", m, err)
	}
	recordTwoAcceptedOneRejected(t, m)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	for _, p := range laborAssignmentPoints(t, &rm) {
		outcome, ok := p.Attributes.Value(laborOutcomeKey)
		if !ok {
			t.Error("point carries no plain outcome attribute (ADR-0015 Tier 2)")
			continue
		}
		reason, hasReason := p.Attributes.Value(laborReasonKey)
		if outcome.AsString() == laborOutcomeRejected {
			if !hasReason || reason.AsString() != "uncertified" {
				t.Errorf("rejected point reason = %v, want %q", reason.AsString(), "uncertified")
			}
		} else if hasReason {
			t.Error("accepted point carries a reason attribute; it should not")
		}
	}
}

// The adapter implements the application port — a compile-time guarantee
// the hexagonal rule (application never imports otel) stays enforceable.
var _ ports.LaborMetrics = (*LaborMetrics)(nil)
