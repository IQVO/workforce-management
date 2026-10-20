package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
)

type nopUnitOfWork struct{}

func (nopUnitOfWork) Execute(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

// The regression this guards: cmd/mcp used to build AssignLabor with no
// UnitOfWork and a log-only publisher, so MCP-initiated assignments never
// reached Kafka (ADR-0008/0016). Every use case must share the publisher,
// UnitOfWork and catalogue the composition root resolved.
func TestBuildDeps_SharesPublisherUnitOfWorkAndCatalogue(t *testing.T) {
	pub := events.NewLogPublisher(quietLogger())
	uow := nopUnitOfWork{}
	catalogue := pathcatalog.New([]pathcatalog.PathDefinition{{Id: "PACK", MatchPrefix: "pack"}})
	r := repos{
		associates:  memory.NewAssociateRepo(),
		shiftPlans:  memory.NewShiftPlanRepo(),
		assignments: memory.NewAssignmentRepo(),
		uow:         uow,
	}

	deps := buildDeps(r, pub, catalogue, 8, quietLogger())

	if deps.AssignLabor == nil || deps.GetStaffingGap == nil || deps.ProposePathPlan == nil {
		t.Fatal("a use case is not wired")
	}
	if deps.AssignLabor.Events != pub || deps.GetStaffingGap.Events != pub || deps.ProposePathPlan.Events != pub {
		t.Error("use cases must share the composed publisher")
	}
	if deps.AssignLabor.UnitOfWork != uow || deps.GetStaffingGap.UnitOfWork != uow || deps.ProposePathPlan.UnitOfWork != uow {
		t.Error("use cases must share the composed UnitOfWork")
	}
	if deps.AssignLabor.MaxHoursPerShift != 8 {
		t.Errorf("MaxHoursPerShift = %v, want 8", deps.AssignLabor.MaxHoursPerShift)
	}
	if deps.Catalogue != catalogue {
		t.Error("Deps.Catalogue must be the composed catalogue (ADR-0013 validation on MCP path ids)")
	}
}

// Without DATABASE_URL there is no in-memory repository to fall back to: the
// error names the variable (ADR-0038).
func TestNewRepos_RefusesAnEmptyDatabaseURL(t *testing.T) {
	r, closeRepos, err := newRepos(context.Background(), quietLogger(), "", "", "")
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("newRepos with no DATABASE_URL = %v, want a boot error naming it", err)
	}
	closeRepos()
	if r.pool != nil || r.uow != nil || r.associates != nil {
		t.Errorf("a refused newRepos must wire nothing, got %+v", r)
	}
}

func TestPublisherConfigFromEnv(t *testing.T) {
	cfg := publisherConfigFromEnv([]string{"a:9092", "b:9092"})
	if len(cfg.Brokers) != 2 || cfg.Brokers[1] != "b:9092" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.RunRelay {
		t.Error("cmd/mcp must never run the outbox relay: only cmd/workforce drains outbox_events")
	}
}

func TestCatalogueConfigFromEnv(t *testing.T) {
	cfg := catalogueConfigFromEnv([]string{"k:9092"})
	if len(cfg.Brokers) != 1 || cfg.Brokers[0] != "k:9092" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestCloseCatalogue_NilIsSafe(t *testing.T) {
	closeCatalogue(nil)
}

// ADR-0015 Tier 1: cmd/mcp serves HTTP, so it emits
// http.server.request.duration and request spans like every other fleet
// HTTP server.
func TestNewRouter_EmitsStandardHTTPServerMetricsAndSpans(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prevMeter := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prevMeter) })

	recorder := tracetest.NewSpanRecorder()
	prevTracer := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(prevTracer) })

	router := newTestRouter(t) // built AFTER the providers are installed

	for _, path := range []string{"/healthz", "/"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if path == "/healthz" {
			req = httptest.NewRequest(http.MethodGet, path, nil)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		router.ServeHTTP(httptest.NewRecorder(), req)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = true
		}
	}
	if !got["http.server.request.duration"] {
		t.Errorf("http.server.request.duration not emitted by cmd/mcp's router; got %v", got)
	}
	if len(recorder.Ended()) == 0 {
		t.Error("cmd/mcp's router started no request span (otelchi missing)")
	}
}

func TestEnvFloatOrDefault(t *testing.T) {
	t.Setenv("ADRFIX_FLOAT", "")
	if got := envFloatOrDefault("ADRFIX_FLOAT", 8); got != 8 {
		t.Errorf("unset = %v, want default 8", got)
	}
	t.Setenv("ADRFIX_FLOAT", "7.5")
	if got := envFloatOrDefault("ADRFIX_FLOAT", 8); got != 7.5 {
		t.Errorf("set = %v, want 7.5", got)
	}
}
