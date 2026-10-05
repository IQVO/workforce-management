package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// laborMeterName is this adapter's OpenTelemetry instrumentation scope for
// the labor business counter (ADR-0015 Tier 2). It moved here from
// internal/application/usecases (which must not import otel) as part of the
// 2026-10 ADR-conformance pass; the instrument NAME is unchanged.
const laborMeterName = "github.com/claudioed/workforce-management/internal/adapters/outbound/telemetry/labor"

// laborAssignmentsCounterName is the business metric: how much labor is
// being assigned to paths, and why assignments are being rejected. A
// rejection rate climbing towards the acceptance rate means the floor is
// being told to staff paths it cannot (missing certifications, associates
// out of hours), which is exactly the question this counter exists to
// answer from a dashboard alone.
const laborAssignmentsCounterName = "workforce.labor_assignments"

// Attribute keys per ADR-0015 Tier 2: an attribute that identifies *what
// happened* is always named outcome; one that identifies *why* is always
// named reason. (Before the 2026-10 conformance pass these were
// workforce.assignment.outcome / workforce.assignment.reason; the rename is
// noted in ADR-0015's status note.)
const (
	laborOutcomeKey = attribute.Key("outcome")
	laborReasonKey  = attribute.Key("reason")
	laborPathKey    = attribute.Key("workforce.path.id")

	laborOutcomeAccepted = "accepted"
	laborOutcomeRejected = "rejected"
)

// LaborMetrics implements ports.LaborMetrics against the global
// MeterProvider — the same provider telemetry.Setup installs, so until
// Setup runs (tests, local dev) recording is a cheap no-op.
type LaborMetrics struct {
	counter metric.Int64Counter
}

var _ ports.LaborMetrics = (*LaborMetrics)(nil)

// NewLaborMetrics registers the workforce.labor_assignments counter. It
// only fails on an invalid instrument name (a programming error), so a
// caller that would rather run un-instrumented than not at all can ignore
// the error and pass a nil ports.LaborMetrics instead.
func NewLaborMetrics() (*LaborMetrics, error) {
	counter, err := otel.Meter(laborMeterName).Int64Counter(
		laborAssignmentsCounterName,
		metric.WithDescription("Labor assignment attempts, by outcome, rejection reason and path."),
		metric.WithUnit("{assignment}"),
	)
	if err != nil {
		return nil, err
	}
	return &LaborMetrics{counter: counter}, nil
}

// AssignmentAccepted implements ports.LaborMetrics.
func (m *LaborMetrics) AssignmentAccepted(ctx context.Context, pathId shared.PathId) {
	m.counter.Add(ctx, 1, metric.WithAttributes(
		laborPathKey.String(string(pathId)),
		laborOutcomeKey.String(laborOutcomeAccepted),
	))
}

// AssignmentRejected implements ports.LaborMetrics.
func (m *LaborMetrics) AssignmentRejected(ctx context.Context, pathId shared.PathId, reason string) {
	m.counter.Add(ctx, 1, metric.WithAttributes(
		laborPathKey.String(string(pathId)),
		laborOutcomeKey.String(laborOutcomeRejected),
		laborReasonKey.String(reason),
	))
}
