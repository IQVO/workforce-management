// Command a2a is the composition root for the Workforce Management A2A
// agent: it wires env config to outbound adapters, adapters to the use cases,
// and those to the inbound A2A adapter, then serves Agent2Agent JSON-RPC over
// HTTP with the Agent Card at the well-known path. It is a third, independent
// deployable alongside cmd/workforce (the HTTP service) and cmd/mcp, per
// ADR-0039.
//
// Like cmd/workforce it runs on real Postgres and real Kafka only (ADR-0038):
// there is no in-memory mode, no log publisher and no file catalogue. A
// missing mandatory variable is a boot error naming it, raised before
// telemetry, any connection and the listener. It never starts the outbox relay
// (only cmd/workforce drains outbox_events) and writes only outbox rows, but
// it needs Kafka to replay the process-path catalogue its skills validate
// path ids against, so KAFKA_BROKERS is mandatory here too.
//
// Environment:
//
//	DATABASE_URL             REQUIRED. Postgres DSN
//	KAFKA_BROKERS            REQUIRED. Comma-separated brokers
//	A2A_AGENT_URL            REQUIRED. Absolute http(s) URL of this agent's
//	                         JSON-RPC endpoint, advertised on the Agent Card
//	MIGRATIONS_DATABASE_URL  direct DSN for the boot migrations (default DATABASE_URL)
//	EVENT_PUBLISHER          kafka (default). log is REJECTED at boot
//	PATH_CATALOGUE_SOURCE    kafka (default). file is REJECTED at boot
//	A2A_ADDR                 listen address (default :8093)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inbounda2a "github.com/claudioed/workforce-management/internal/adapters/inbound/a2a"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/bootretry"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/clock"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/kafkacatalog"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/telemetry"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/composition"
)

func main() {
	if err := run(); err != nil {
		slog.Error("a2a agent exited with error", "error", err)
		os.Exit(1)
	}
}

// setupTelemetry wires OTel with the same non-blocking discipline as the
// HTTP service: an unreachable Collector degrades to dropped telemetry,
// never a server that won't start. It returns the resolved service name
// (the router's metrics label) and a func that flushes on shutdown (a no-op
// when setup degraded to nil).
func setupTelemetry(ctx context.Context, logger *slog.Logger) (string, func()) {
	serviceName := envOrDefault("OTEL_SERVICE_NAME", "workforce-management-a2a")
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

// repos bundles the persistence ports the A2A skills share. pool and uow are
// always set: Postgres is the only runtime (ADR-0038).
type repos struct {
	associates  ports.AssociateRepo
	shiftPlans  ports.ShiftPlanRepo
	assignments ports.AssignmentRepo
	pool        *pgxpool.Pool
	uow         ports.UnitOfWork
}

// bootConfig is the mandatory configuration of cmd/mcp: real Postgres and
// real Kafka, with no default and no degraded mode behind any of it.
type bootConfig struct {
	databaseURL string
	brokers     []string
	agentURL    string
}

// errNoAgentURL is the boot error for a missing A2A_AGENT_URL: the Agent Card
// must advertise the absolute URL clients dial (ADR-0039).
var errNoAgentURL = errors.New("A2A_AGENT_URL is required: the Agent Card must advertise the absolute URL of this agent's JSON-RPC endpoint (ADR-0039), e.g. http://<release>-a2a.<ns>.svc.cluster.local:8093/")

// checkAgentURL validates A2A_AGENT_URL: an absolute http(s) URL. The card is
// the discovery contract — a relative or schemeless URL is a misconfiguration
// every client would hit, so it is refused at boot, never silently served.
func checkAgentURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errNoAgentURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("A2A_AGENT_URL=%q is not an absolute http(s) URL: the Agent Card must advertise the endpoint clients dial (ADR-0039)", raw)
	}
	return nil
}

// loadBootConfig reads and validates the mandatory environment. Every problem
// is reported at once, each naming its variable, so the process fails fast at
// boot, before telemetry, any connection and the listener.
func loadBootConfig() (bootConfig, error) {
	cfg := bootConfig{
		databaseURL: os.Getenv("DATABASE_URL"),
		brokers:     composition.ParseBrokers(os.Getenv("KAFKA_BROKERS")),
		agentURL:    strings.TrimSpace(os.Getenv("A2A_AGENT_URL")),
	}
	var errs []error
	if cfg.databaseURL == "" {
		errs = append(errs, composition.ErrNoDatabase)
	}
	if len(cfg.brokers) == 0 {
		errs = append(errs, composition.ErrNoBrokers)
	}
	errs = append(errs,
		composition.CheckEventPublisher(os.Getenv("EVENT_PUBLISHER")),
		composition.CheckCatalogueSource(os.Getenv("PATH_CATALOGUE_SOURCE")),
		checkAgentURL(cfg.agentURL),
	)
	if err := errors.Join(errs...); err != nil {
		return bootConfig{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// newRepos migrates then connects a pgx pool and wires the Postgres
// repositories. An empty databaseURL is a boot error: there is no in-memory
// fallback. The returned close func releases the pool.
//
// migrationsDatabaseURL is used ONLY for the golang-migrate step below —
// the pgxpool opened just after it (databaseURL) is unchanged. See
// cmd/workforce/main.go's openPostgresPool doc comment for the full "why"
// a direct, non-pooled connection is needed here even though the pgxpool
// stays on PgBouncer (ADR 0025-migrations-direct-postgres-connection.md,
// ported from order-management's ADR-0029).
func newRepos(ctx context.Context, logger *slog.Logger, databaseURL, migrationsDatabaseURL, migrationsPath string) (repos, func(), error) {
	if databaseURL == "" {
		return repos{}, func() {}, composition.ErrNoDatabase
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

// publisherConfigFromEnv builds the publisher settings for the shared
// composition.BuildEventPublisher (KAFKA_BROKERS arrives validated). RunRelay
// is false: only cmd/workforce drains outbox_events onto Kafka; this process
// only inserts.
func publisherConfigFromEnv(brokers []string) composition.PublisherConfig {
	return composition.PublisherConfig{
		Brokers:  brokers,
		RunRelay: false,
	}
}

// catalogueConfigFromEnv builds the same catalogue settings cmd/workforce
// uses, so A2A skills validate path ids against the same catalogue
// (ADR-0013): replayed from Kafka (KAFKA_BROKERS arrives validated).
func catalogueConfigFromEnv(brokers []string) composition.CatalogueConfig {
	return composition.CatalogueConfig{Brokers: brokers}
}

// newLaborMetrics wires the SAME workforce.labor_assignments Tier-2 counter
// (ADR-0015) cmd/workforce gives its HTTP-triggered AssignLabor, so
// A2A-triggered assignments are counted on the identical instrument.
// Errors are non-fatal: nil means "not instrumented".
func newLaborMetrics(logger *slog.Logger) ports.LaborMetrics {
	laborMetrics, err := telemetry.NewLaborMetrics()
	if err != nil {
		logger.Warn("labor assignment metrics unavailable; A2A assignments will run without the workforce.labor_assignments counter", "error", err)
	}
	return laborMetrics
}

// buildDeps wires the A2A adapter's use cases with the SAME publisher, clock,
// UnitOfWork and catalogue the HTTP service gives them: GetStaffingGap and
// ProposePathPlan (read) and AssignLabor (write). AssignLabor also gets the
// SAME Tier-2 metrics port (ADR-0015): A2A-triggered assignments count on
// workforce.labor_assignments exactly like HTTP-triggered ones.
func buildDeps(r repos, publisher ports.EventPublisher, catalogue ports.PathCatalogue, maxHoursPerShift float64, logger *slog.Logger) inbounda2a.Deps {
	sysClock := clock.System{}
	return inbounda2a.Deps{
		GetStaffingGap:  &usecases.GetStaffingGap{ShiftPlans: r.shiftPlans, Assignments: r.assignments, Events: publisher, Clock: sysClock, UnitOfWork: r.uow},
		ProposePathPlan: &usecases.ProposePathPlan{Events: publisher, Clock: sysClock, UnitOfWork: r.uow},
		AssignLabor:     &usecases.AssignLabor{Associates: r.associates, Assignments: r.assignments, Events: publisher, Clock: sysClock, MaxHoursPerShift: maxHoursPerShift, Catalogue: catalogue, Metrics: newLaborMetrics(logger), UnitOfWork: r.uow},
		Catalogue:       catalogue,
	}
}

// serveA2A runs srv until it fails or SIGINT/SIGTERM arrives, shutting the
// server down gracefully on a signal.
func serveA2A(logger *slog.Logger, srv *http.Server, httpAddr string) error {
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("a2a agent listening (JSON-RPC over HTTP)", "addr", httpAddr)
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

	// Fail fast, before telemetry, any connection and the listener.
	cfg, err := loadBootConfig()
	if err != nil {
		return err
	}

	ctx := context.Background()
	serviceName, flushTelemetry := setupTelemetry(ctx, logger)
	defer flushTelemetry()

	httpAddr := envOrDefault("A2A_ADDR", ":8093")
	databaseURL := cfg.databaseURL
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
	// cmd/workforce (ADR-0013): A2A path ids are validated against it. Its
	// kafka-source consumer must outlive this call, so it gets its own
	// cancellable context.
	catalogueCtx, cancelCatalogue := context.WithCancel(context.Background())
	defer cancelCatalogue()
	catalogue, kafkaCatalogue, err := composition.BuildCatalogue(ctx, catalogueCtx, catalogueConfigFromEnv(cfg.brokers), logger)
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
	// LaborReassigned exactly like the REST path does (ADR-0026): through the
	// same composition.BuildEventPublisher wiring cmd/workforce uses, with the
	// same UnitOfWork. The events go into outbox_events (integration +
	// analytics topics) in the use case's transaction; the outbox relay keeps
	// running only in cmd/workforce.
	built, err := composition.BuildEventPublisher(publisherConfigFromEnv(cfg.brokers), r.pool, r.shiftPlans, logger)
	if err != nil {
		return err
	}
	defer built.Close()

	executor := inbounda2a.NewExecutor(buildDeps(r, built.Publisher, catalogue, maxHoursPerShift, logger))
	card := inbounda2a.Card(inbounda2a.Config{AgentURL: cfg.agentURL, Version: serviceVersion()})

	srv := &http.Server{Addr: httpAddr, Handler: newRouter(inbounda2a.Handler(executor), card, serviceName), ReadHeaderTimeout: 5 * time.Second}

	return serveA2A(logger, srv, httpAddr)
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
