// Command mcp is the composition root for the Workforce Management MCP
// server: it wires env config to outbound adapters, adapters to the use
// cases, and those to the inbound MCP adapter, then serves MCP over Streamable
// HTTP. It is a second, independent deployable alongside cmd/workforce (the
// HTTP service), per ADR-0008.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inboundmcp "github.com/claudioed/workforce-management/internal/adapters/inbound/mcp"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/bootretry"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/clock"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/kafkacatalog"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/telemetry"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/composition"
)

func main() {
	if err := run(); err != nil {
		slog.Error("mcp server exited with error", "error", err)
		os.Exit(1)
	}
}

// setupTelemetry wires OTel with the same non-blocking discipline as the
// HTTP service: an unreachable Collector degrades to dropped telemetry,
// never a server that won't start. It returns the resolved service name
// (the router's metrics label) and a func that flushes on shutdown (a no-op
// when setup degraded to nil).
func setupTelemetry(ctx context.Context, logger *slog.Logger) (string, func()) {
	serviceName := envOrDefault("OTEL_SERVICE_NAME", "workforce-management-mcp")
	otlpEndpoint := envOrDefault("OTEL_EXPORTER_OTLP_ENDPOINT", telemetry.DefaultOTLPEndpoint)
	shutdownTelemetry, err := telemetry.Setup(ctx, serviceName, serviceVersion(), otlpEndpoint)
	if err != nil {
		logger.Error("opentelemetry setup degraded", "error", err)
	}
	if shutdownTelemetry != nil {
		return serviceName, func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdownTelemetry(shutdownCtx); err != nil {
				logger.Warn("telemetry shutdown did not flush cleanly", "error", err)
			}
		}
	}
	return serviceName, func() {}
}

// repos bundles the persistence ports the MCP tools share. pool and uow are
// nil in the in-memory configuration (no DATABASE_URL): the use cases then
// run Save and Publish back to back (see ports.UnitOfWork).
type repos struct {
	associates  ports.AssociateRepo
	shiftPlans  ports.ShiftPlanRepo
	assignments ports.AssignmentRepo
	pool        *pgxpool.Pool
	uow         ports.UnitOfWork
}

// newRepos selects in-memory vs Postgres repos the same way the platform
// does: no DATABASE_URL means local/in-memory adapters; a URL means migrate
// then connect a pgx pool. The returned close func releases the pool when
// one was opened.
//
// migrationsDatabaseURL is used ONLY for the golang-migrate step below —
// the pgxpool opened just after it (databaseURL) is unchanged. See
// cmd/workforce/main.go's openPostgresPool doc comment for the full "why"
// a direct, non-pooled connection is needed here even though the pgxpool
// stays on PgBouncer (ADR 0025-migrations-direct-postgres-connection.md,
// ported from order-management's ADR-0029).
func newRepos(ctx context.Context, logger *slog.Logger, databaseURL, migrationsDatabaseURL, migrationsPath string) (repos, func(), error) {
	if databaseURL == "" {
		logger.Info("database url not configured; using in-memory adapters")
		return repos{
			associates:  memory.NewAssociateRepo(),
			shiftPlans:  memory.NewShiftPlanRepo(),
			assignments: memory.NewAssignmentRepo(),
		}, func() {}, nil
	}
	// Retried: in this fleet EVERY injected pod's FIRST outbound TCP
	// dial (here, Postgres) is reset ~10s after the app starts
	// (Istio native sidecars). A single attempt turns that known,
	// transient condition into CrashLoopBackOff. The retry does not
	// weaken the fail-closed rule: once the budget is exhausted
	// this still refuses to boot.
	if err := bootretry.Retry(ctx, logger, "run migrations", func() error {
		return postgres.Migrate(migrationsDatabaseURL, migrationsPath)
	}); err != nil {
		return repos{}, nil, err
	}
	var pool *pgxpool.Pool
	if err := bootretry.Retry(ctx, logger, "open postgres pool", func() error {
		var err error
		pool, err = postgres.NewPool(ctx, databaseURL)
		return err
	}); err != nil {
		return repos{}, nil, err
	}
	return repos{
		associates:  postgres.NewAssociateRepo(pool),
		shiftPlans:  postgres.NewShiftPlanRepo(pool),
		assignments: postgres.NewAssignmentRepo(pool),
		pool:        pool,
		uow:         postgres.NewUnitOfWork(pool),
	}, func() { pool.Close() }, nil
}

// publisherConfigFromEnv resolves EVENT_PUBLISHER, KAFKA_BROKERS and
// OUTBOX_RELAY_INTERVAL for the shared composition.BuildEventPublisher.
// RunRelay is false: only cmd/workforce drains outbox_events onto Kafka;
// this process only inserts.
func publisherConfigFromEnv() composition.PublisherConfig {
	return composition.PublisherConfig{
		Kind:     envOrDefault("EVENT_PUBLISHER", composition.LogPublisher),
		Brokers:  strings.Split(envOrDefault("KAFKA_BROKERS", "localhost:9092"), ","),
		RunRelay: false,
	}
}

// catalogueConfigFromEnv resolves the same PATH_CATALOGUE_* settings
// cmd/workforce uses, so MCP tools validate path ids against the same
// catalogue (ADR-0013).
func catalogueConfigFromEnv() composition.CatalogueConfig {
	return composition.CatalogueConfig{
		Source:  envOrDefault("PATH_CATALOGUE_SOURCE", composition.CatalogueFromFile),
		File:    envOrDefault("PATH_CATALOGUE_FILE", "/etc/workforce-management/process-paths.yaml"),
		Brokers: strings.Split(os.Getenv("KAFKA_BROKERS"), ","),
	}
}

// newLaborMetrics wires the SAME workforce.labor_assignments Tier-2 counter
// (ADR-0015) cmd/workforce gives its HTTP-triggered AssignLabor, so
// MCP-triggered assignments are counted on the identical instrument.
// Errors are non-fatal: nil means "not instrumented".
func newLaborMetrics(logger *slog.Logger) ports.LaborMetrics {
	laborMetrics, err := telemetry.NewLaborMetrics()
	if err != nil {
		logger.Warn("labor assignment metrics unavailable; MCP assignments will run without the workforce.labor_assignments counter", "error", err)
	}
	return laborMetrics
}

// buildDeps wires the MCP adapter's use cases with the SAME publisher, clock,
// UnitOfWork and catalogue the HTTP service gives them: GetStaffingGap and
// ProposePathPlan (read) and AssignLabor (write). AssignLabor also gets the
// SAME Tier-2 metrics port (ADR-0015): MCP-triggered assignments count on
// workforce.labor_assignments exactly like HTTP-triggered ones.
func buildDeps(r repos, publisher ports.EventPublisher, catalogue ports.PathCatalogue, maxHoursPerShift float64, logger *slog.Logger) inboundmcp.Deps {
	sysClock := clock.System{}
	return inboundmcp.Deps{
		GetStaffingGap:  &usecases.GetStaffingGap{ShiftPlans: r.shiftPlans, Assignments: r.assignments, Events: publisher, Clock: sysClock, UnitOfWork: r.uow},
		ProposePathPlan: &usecases.ProposePathPlan{Events: publisher, Clock: sysClock, UnitOfWork: r.uow},
		AssignLabor:     &usecases.AssignLabor{Associates: r.associates, Assignments: r.assignments, Events: publisher, Clock: sysClock, MaxHoursPerShift: maxHoursPerShift, Metrics: newLaborMetrics(logger), UnitOfWork: r.uow},
		Catalogue:       catalogue,
	}
}

// serveMCP runs srv until it fails or SIGINT/SIGTERM arrives, shutting the
// server down gracefully on a signal.
func serveMCP(logger *slog.Logger, srv *http.Server, httpAddr string) error {
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("mcp server listening (Streamable HTTP)", "addr", httpAddr)
		serverErr <- srv.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	case <-stop:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}

func run() error {
	logger := newLogger(envOrDefault("LOG_LEVEL", "info"))
	slog.SetDefault(logger)

	ctx := context.Background()
	serviceName, flushTelemetry := setupTelemetry(ctx, logger)
	defer flushTelemetry()

	httpAddr := envOrDefault("MCP_ADDR", ":8090")
	databaseURL := os.Getenv("DATABASE_URL")
	// See cmd/workforce/main.go's openPostgresPool doc comment for the
	// full "why" (session-scoped pg_advisory_lock vs PgBouncer
	// transaction-pooling incompatibility, ADR
	// 0025-migrations-direct-postgres-connection.md; ported from
	// order-management's ADR-0029). This binary also runs migrations on
	// start (newRepos below), so it needs the same direct-connection
	// split. Falls back to databaseURL when unset.
	migrationsDatabaseURL := envOrDefault("MIGRATIONS_DATABASE_URL", databaseURL)
	migrationsPath := envOrDefault("MIGRATIONS_PATH", "migrations")
	maxHoursPerShift := envFloatOrDefault("MAX_HOURS_PER_SHIFT", 8.0)

	// The process-path catalogue is boot-time-required here exactly as in
	// cmd/workforce (ADR-0013): MCP path ids are validated against it. Its
	// kafka-source consumer must outlive this call, so it gets its own
	// cancellable context.
	catalogueCtx, cancelCatalogue := context.WithCancel(context.Background())
	defer cancelCatalogue()
	catalogue, kafkaCatalogue, err := composition.BuildCatalogue(ctx, catalogueCtx, catalogueConfigFromEnv(), logger)
	if err != nil {
		return err
	}
	defer closeCatalogue(kafkaCatalogue)

	r, closeRepos, err := newRepos(ctx, logger, databaseURL, migrationsDatabaseURL, migrationsPath)
	if err != nil {
		return err
	}
	defer closeRepos()

	// assign_labor is a WRITE use case, so it must publish LaborAssigned /
	// LaborReassigned exactly like the REST path does (ADR-0008, ADR-0016):
	// through the same composition.BuildEventPublisher wiring cmd/workforce
	// uses, with the same UnitOfWork. With EVENT_PUBLISHER=kafka and
	// DATABASE_URL the events go into outbox_events (integration +
	// analytics topics) in the use case's transaction; the outbox relay keeps
	// running only in cmd/workforce.
	built, err := composition.BuildEventPublisher(publisherConfigFromEnv(), r.pool, r.shiftPlans, logger)
	if err != nil {
		return err
	}
	defer built.Close()

	deps := buildDeps(r, built.Publisher, catalogue, maxHoursPerShift, logger)
	// Optional curated data-product tool: when REPORTS_BASE_URL is set, the MCP
	// server exposes get_workforce_labor_report backed by the workforce-reports
	// REST service (ADR-0010). It never opens the analytical database directly.
	if reportsBaseURL := os.Getenv("REPORTS_BASE_URL"); reportsBaseURL != "" {
		deps.Reports = inboundmcp.NewReportsRESTClient(reportsBaseURL, nil)
		logger.Info("reports data-product tool enabled", "reports_base_url", reportsBaseURL)
	}
	server := inboundmcp.NewServer(deps)

	// The MCP handler is mounted at / and /mcp behind a GET /healthz so
	// Kubernetes probes can reach the process (see router.go).
	handler := newRouter(inboundmcp.Handler(server), serviceName)

	srv := &http.Server{Addr: httpAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	return serveMCP(logger, srv, httpAddr)
}

// closeCatalogue releases the kafka-sourced catalogue consumer, if any.
func closeCatalogue(c *kafkacatalog.Consumer) {
	if c != nil {
		_ = c.Close()
	}
}

// version is the service version reported as the OTel service.version
// resource attribute, overridable at build time (-ldflags "-X main.version=...")
// and otherwise falling back to the SERVICE_VERSION env var, then "dev".
var version = ""

func serviceVersion() string {
	if version != "" {
		return version
	}
	return envOrDefault("SERVICE_VERSION", "dev")
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(telemetry.NewTraceHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}),
	))
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloatOrDefault(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Error("invalid float env var", "key", key, "error", err)
		os.Exit(1)
	}
	return f
}
