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
	"github.com/claudioed/workforce-management/internal/composition"
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

// Without DATABASE_URL there is no pool and no UnitOfWork (in-memory mode).
func TestNewRepos_InMemoryHasNoPoolOrUnitOfWork(t *testing.T) {
	r, closeRepos, err := newRepos(context.Background(), quietLogger(), "", "", "")
	if err != nil {
		t.Fatalf("newRepos: %v", err)
	}
	defer closeRepos()
	if r.pool != nil || r.uow != nil {
		t.Errorf("in-memory repos must carry no pool/uow, got pool=%v uow=%v", r.pool, r.uow)
	}
}

func TestPublisherConfigFromEnv(t *testing.T) {
	t.Setenv("EVENT_PUBLISHER", "")
	t.Setenv("KAFKA_BROKERS", "")
	cfg := publisherConfigFromEnv()
	if cfg.Kind != composition.LogPublisher || cfg.RunRelay {
		t.Errorf("defaults = %+v, want log publisher and no relay", cfg)
	}

	t.Setenv("EVENT_PUBLISHER", "kafka")
	t.Setenv("KAFKA_BROKERS", "a:9092,b:9092")
	cfg = publisherConfigFromEnv()
	if cfg.Kind != composition.KafkaPublisher || len(cfg.Brokers) != 2 || cfg.Brokers[1] != "b:9092" {
		t.Errorf("kafka cfg = %+v", cfg)
	}
	if cfg.RunRelay {
		t.Error("cmd/mcp must never run the outbox relay: only cmd/workforce drains outbox_events")
	}
}

func TestCatalogueConfigFromEnv(t *testing.T) {
	t.Setenv("PATH_CATALOGUE_SOURCE", "")
	t.Setenv("PATH_CATALOGUE_FILE", "")
	cfg := catalogueConfigFromEnv()
	if cfg.Source != composition.CatalogueFromFile || cfg.File != "/etc/workforce-management/process-paths.yaml" {
		t.Errorf("defaults = %+v", cfg)
	}
	t.Setenv("PATH_CATALOGUE_SOURCE", "kafka")
	t.Setenv("PATH_CATALOGUE_FILE", "/tmp/x.yaml")
	t.Setenv("KAFKA_BROKERS", "k:9092")
	cfg = catalogueConfigFromEnv()
	if cfg.Source != composition.CatalogueFromKafka || cfg.File != "/tmp/x.yaml" || cfg.Brokers[0] != "k:9092" {
		t.Errorf("overrides = %+v", cfg)
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
