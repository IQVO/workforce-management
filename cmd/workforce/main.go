// Command workforce is the composition root for the Workforce Management
// service: it wires config from the environment to adapters, use cases, and
// the HTTP router, then serves.
//
// Postgres and Kafka are the ONLY runtime (ADR-0038): there is no in-memory
// mode, no log publisher, no file catalogue and no permissive consumer mode.
// A missing or rejected mandatory variable is a boot error naming it, raised
// before the listener binds. The service never creates topics through an
// admin API: the cluster broker auto-creates them on first write.
//
// Environment (the mandatory ones are REQUIRED, with no default):
//
//	DATABASE_URL                REQUIRED. Postgres DSN
//	KAFKA_BROKERS               REQUIRED. Comma-separated brokers
//	MIGRATIONS_DATABASE_URL     direct DSN for the boot migrations (default DATABASE_URL)
//	EVENT_PUBLISHER             kafka (default). log is REJECTED at boot
//	PATH_CATALOGUE_SOURCE       kafka (default). file is REJECTED at boot
//	LABOR_PERFORMANCE_MODE      kafka-cache (default) | http (REST rollback). permissive is REJECTED
//	INSTALLED_CAPACITY_MODE     http | permissive (REST edge, unchanged by ADR-0038)
//	HTTP_ADDR                   listen address (default :8080)
//	OUTBOX_RELAY_INTERVAL       relay poll interval (default 1s)
//	OUTBOX_MAX_ATTEMPTS         failures before the relay excludes a row (default 20, 0 = unlimited)
//	SHUTDOWN_DRAIN_DELAY        wait after flipping /readyz before closing (default 10s)
//
// /readyz is the drain gate AND a Postgres ping AND a Kafka metadata check
// (a broker answers a request that names no topic: reachability only, never
// topics), cached for a few seconds. /healthz is liveness only.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inbound "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/bootretry"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/clock"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/fulfillmentexecution"
	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/kafkacatalog"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/laborperformance"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/laborperformancecache"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/telemetry"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/composition"
	"github.com/claudioed/workforce-management/internal/resilience"
)

// version is the service version reported as the OTel `service.version`
// resource attribute. It is overridable at build time
// (-ldflags "-X main.version=1.2.3") and otherwise falls back to the
// SERVICE_VERSION env var, then "dev".
var version = ""

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("service exited with error", "error", err)
		os.Exit(1)
	}
}

// newLogger builds the service's JSON-to-stdout slog.Logger. level accepts
// debug|info|warn|warning|error (case-insensitive; warning is an alias of
// warn, as in every other binary of the fleet), defaulting to info for an
// unrecognized value.
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
	// TraceHandler wraps the JSON handler so any *Context log call made while
	// a span is active also carries trace_id/span_id.
	return slog.New(telemetry.NewTraceHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}),
	))
}

// serviceVersion resolves service.version: build-time ldflags first, then
// SERVICE_VERSION, then "dev".
func serviceVersion() string {
	if version != "" {
		return version
	}
	return envOrDefault("SERVICE_VERSION", "dev")
}

// run boots the service and serves until SIGINT/SIGTERM or until ctx is
// cancelled (the in-process tests stop it that way), then shuts down in the
// ADR-0022 §8 order. It reads the whole configuration from the environment.
func run(ctx context.Context) error {
	logger := newLogger(envOrDefault("LOG_LEVEL", "info"))
	slog.SetDefault(logger)

	// Fail fast, before telemetry, any connection and the listener.
	cfg, err := loadBootConfig()
	if err != nil {
		return err
	}

	serviceName, shutdownTelemetry, err := setupServiceTelemetry(ctx, logger)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			logger.Warn("telemetry shutdown did not flush cleanly", "error", err)
		}
	}()

	breakerMetrics := newBreakerMetrics(logger)

	settings := envSettingsFromEnv(cfg.databaseURL)

	// catalogueConsumerCtx/cancelCatalogueConsumer are declared here
	// (rather than deferred to later in run()) because the Kafka
	// catalogue source needs its own Run goroutine started BEFORE
	// WaitReady is called below -- otherwise nothing would ever be
	// consuming messages while this process waits, guaranteeing a
	// deadlock until WaitReadyTimeout.
	catalogueConsumerCtx, cancelCatalogueConsumer := context.WithCancel(context.Background())
	defer cancelCatalogueConsumer()

	catalogue, kafkaCatalogue, err := composition.BuildCatalogue(ctx, catalogueConsumerCtx, composition.CatalogueConfig{Brokers: cfg.brokers}, logger)
	if err != nil {
		return err
	}

	pool, err := openPostgresPool(ctx, logger, settings.databaseURL, settings.migrationsDatabaseURL, settings.migrationsPath)
	if err != nil {
		return err
	}
	defer pool.Close()

	repos, closeRepos, err := wireRepos(pool, cfg.brokers, logger)
	if err != nil {
		return err
	}
	defer closeRepos()
	sysClock := clock.System{}

	rateConsumerCtx, cancelRateConsumer := context.WithCancel(context.Background())
	defer cancelRateConsumer()
	measuredRate, idleShare, kafkaMeasuredRate, err := newMeasuredRateClients(ctx, rateConsumerCtx, cfg, breakerMetrics, logger)
	if err != nil {
		return err
	}
	logTopics(logger, cfg)

	// readiness gates GET /readyz (ADR-0022 §graceful shutdown, ported
	// from order-management's ADR-0025, grown by ADR-0038): the drain gate
	// AND a Postgres ping AND a Kafka metadata check. Flipped not-ready as
	// the FIRST step of the shutdown sequence below.
	readiness := buildReadiness(pool, cfg.brokers)

	handler := newOLTPHandler(oltpHandlerDeps{
		associates:       repos.associates,
		shiftPlans:       repos.shiftPlans,
		assignments:      repos.assignments,
		publisher:        repos.publisher,
		sysClock:         sysClock,
		uow:              repos.uow,
		measuredRate:     measuredRate,
		idleShare:        idleShare,
		breakerMetrics:   breakerMetrics,
		maxHoursPerShift: settings.maxHoursPerShift,
		catalogue:        catalogue,
		readiness:        readiness,
		idempotencyPool:  pool,
		logger:           logger,
	})

	server := &http.Server{
		Addr:              settings.httpAddr,
		Handler:           inbound.NewRouter(handler, logger, serviceName),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// The housekeeping sweeper (ADR-0028) bounds idempotency_keys and the
	// published tail of outbox_events. It is stopped in the shutdown
	// sequence BEFORE the pool closes; the deferred call covers the
	// listener-failure path (stop is idempotent).
	stopSweeper := startSweeper(pool, housekeepingSettingsFromEnv(logger), logger)
	defer stopSweeper()

	// Stopping the Kafka consumers is step 4 of the ADR-0022 §8 shutdown
	// sequence, after the HTTP drain.
	stopConsumers := consumerStopper(cancelCatalogueConsumer, kafkaCatalogue, cancelRateConsumer, kafkaMeasuredRate)

	return serveWorkforce(ctx, logger, settings.httpAddr, server, readiness, repos.relay, stopConsumers, stopSweeper)
}

// buildReadiness makes /readyz real: the drain gate AND a Postgres ping AND a
// Kafka metadata check (a broker answers a topic-less metadata request;
// reachability only, never topics: the broker auto-creates them on first
// write). The verdict is cached for inbound.DefaultReadinessTTL.
func buildReadiness(pool *pgxpool.Pool, brokers []string) *inbound.Readiness {
	return inbound.NewReadiness(0, 0,
		inbound.Dependency{Name: "postgres", Check: pool.Ping},
		inbound.Dependency{Name: "kafka", Check: outboundkafka.NewBrokerCheck(brokers).Check},
	)
}

// logTopics logs, once at boot, every topic this process produces to and
// consumes from. It creates none of them: the cluster broker auto-creates a
// topic on the first write and /readyz does not depend on topics existing.
func logTopics(logger *slog.Logger, cfg bootConfig) {
	consumes := []string{kafkacatalog.Topic}
	if cfg.laborMode == laborModeKafkaCache {
		consumes = append(consumes, laborperformancecache.Topic)
	}
	logger.Info("kafka topics",
		"brokers", cfg.brokers,
		"produces", []string{outboundkafka.Topic, outboundkafka.AnalyticsTopic},
		"consumes", consumes,
		"note", "this service never creates topics; the cluster broker auto-creates them on first write")
}

// envSettings groups run()'s plain environment reads so the function itself
// stays inside the linter's length budget.
type envSettings struct {
	databaseURL           string
	migrationsDatabaseURL string
	httpAddr              string
	migrationsPath        string
	maxHoursPerShift      float64
}

// envSettingsFromEnv reads the optional configuration; see openPostgresPool
// and ADR-0025 for MIGRATIONS_DATABASE_URL.
func envSettingsFromEnv(databaseURL string) envSettings {
	return envSettings{
		databaseURL:           databaseURL,
		migrationsDatabaseURL: envOrDefault("MIGRATIONS_DATABASE_URL", databaseURL),
		httpAddr:              envOrDefault("HTTP_ADDR", ":8080"),
		migrationsPath:        envOrDefault("MIGRATIONS_PATH", "migrations"),
		maxHoursPerShift:      envFloatOrDefault("MAX_HOURS_PER_SHIFT", 8.0),
	}
}

// consumerStopper returns the step-4 shutdown action of ADR-0022 §8: cancel
// each Kafka consumer's Run context, then release its reader. The consumers
// are nil when their mode is not kafka.
func consumerStopper(cancelCatalogue context.CancelFunc, catalogue *kafkacatalog.Consumer, cancelRate context.CancelFunc, rate *laborperformancecache.Consumer) func() {
	return func() {
		cancelCatalogue()
		if catalogue != nil {
			_ = catalogue.Close()
		}
		cancelRate()
		if rate != nil {
			_ = rate.Close()
		}
	}
}

// workforceRepos groups the Postgres repositories, the event publisher,
// and the shared UnitOfWork wired at boot.
type workforceRepos struct {
	associates  *postgres.AssociateRepo
	shiftPlans  *postgres.ShiftPlanRepo
	assignments *postgres.AssignmentRepo
	publisher   ports.EventPublisher
	relay       *postgres.OutboxRelay
	uow         ports.UnitOfWork
}

// wireRepos builds the Postgres repositories, the transactional-outbox event
// publisher with its relay, and the shared UnitOfWork. The returned close func
// releases the publisher adapters; the caller defers it so teardown stays LIFO
// with the pool close. Every publishing use case shares one UnitOfWork so its
// Saves and its Publish (an outbox INSERT) commit together (ADR 0016). The
// relay is not optional: a wiring that yields none is a boot error.
func wireRepos(pool *pgxpool.Pool, brokers []string, logger *slog.Logger) (*workforceRepos, func(), error) {
	shiftPlans := postgres.NewShiftPlanRepo(pool)
	built, err := composition.BuildEventPublisher(publisherConfigFromEnv(brokers), pool, shiftPlans, logger)
	if err != nil {
		return nil, nil, err
	}
	if built.Relay == nil {
		built.Close()
		return nil, nil, errors.New("the outbox relay cannot be built: events would never leave the service")
	}
	return &workforceRepos{
		associates:  postgres.NewAssociateRepo(pool),
		shiftPlans:  shiftPlans,
		assignments: postgres.NewAssignmentRepo(pool),
		publisher:   built.Publisher,
		relay:       built.Relay,
		uow:         postgres.NewUnitOfWork(pool),
	}, built.Close, nil
}

// OUTBOX_MAX_ATTEMPTS caps how many failed sends an outbox row may
// accumulate before the relay stops claiming it (gap fix D12, ADR 0036).
// 0 = unlimited.
const (
	outboxMaxAttemptsEnv     = "OUTBOX_MAX_ATTEMPTS"
	defaultOutboxMaxAttempts = 20
)

// publisherConfigFromEnv resolves OUTBOX_RELAY_INTERVAL and
// OUTBOX_MAX_ATTEMPTS (KAFKA_BROKERS arrives validated) for the shared
// composition.BuildEventPublisher. This process owns the outbox relay
// (RunRelay): cmd/mcp only inserts.
func publisherConfigFromEnv(brokers []string) composition.PublisherConfig {
	return composition.PublisherConfig{
		Brokers:       brokers,
		RunRelay:      true,
		RelayInterval: envDurationOrDefault("OUTBOX_RELAY_INTERVAL", time.Second),
		MaxAttempts:   envNonNegativeIntOrDefault(outboxMaxAttemptsEnv, defaultOutboxMaxAttempts),
	}
}

// setupServiceTelemetry wires OTel before any adapter is built and
// returns the resolved service name (the router's metrics label) plus
// the shutdown flush, which the caller defers. A failed final flush is
// logged as a warning, not an error: the usual cause is "no Collector
// listening at OTEL_EXPORTER_OTLP_ENDPOINT", which drops the flush but
// is not a service failure.
func setupServiceTelemetry(ctx context.Context, logger *slog.Logger) (string, func(context.Context) error, error) {
	serviceName := envOrDefault("OTEL_SERVICE_NAME", inbound.DefaultServiceName)
	otlpEndpoint := envOrDefault("OTEL_EXPORTER_OTLP_ENDPOINT", telemetry.DefaultOTLPEndpoint)
	shutdownTelemetry, err := telemetry.Setup(ctx, serviceName, serviceVersion(), otlpEndpoint)
	if err != nil {
		return "", nil, err
	}
	logger.Info("telemetry configured",
		"service_name", serviceName,
		"service_version", serviceVersion(),
		"environment", telemetry.Environment(),
		"otlp_endpoint", otlpEndpoint,
	)
	return serviceName, shutdownTelemetry, nil
}

// newBreakerMetrics wires both outbound breakers' OnStateChange into the
// circuit_breaker.state gauge (ADR-0022), reusing the SAME OTel
// MeterProvider telemetry.Setup already installed rather than standing
// up a second Prometheus registry. Errors here are non-fatal: a nil
// recorder just means this process runs without the gauge, never
// without the breaker itself.
func newBreakerMetrics(logger *slog.Logger) resilience.StateRecorder {
	circuitBreakerMetrics, err := telemetry.NewCircuitBreakerMetrics()
	if err != nil {
		logger.Warn("circuit breaker metrics unavailable; breakers will run without the circuit_breaker.state gauge", "error", err)
	}
	return circuitBreakerMetrics
}

// newLaborMetrics wires the workforce.labor_assignments Tier-2 business
// counter (ADR-0015) behind ports.LaborMetrics. Errors are non-fatal: nil
// means this process runs without the counter, never without assignments.
func newLaborMetrics(logger *slog.Logger) ports.LaborMetrics {
	laborMetrics, err := telemetry.NewLaborMetrics()
	if err != nil {
		logger.Warn("labor assignment metrics unavailable; assignments will run without the workforce.labor_assignments counter", "error", err)
	}
	return laborMetrics
}

// openPostgresPool runs the schema migrations, opens the pool, each
// under boot retry: in this fleet EVERY injected pod's FIRST outbound
// TCP dial (here, Postgres) is reset ~10s after the app starts (Istio
// native sidecars). A single attempt turns that known, transient
// condition into CrashLoopBackOff — migrations fail with "read:
// connection reset by peer", the process exits, and the pod never gets
// far enough to serve its own health probe. The retry does not weaken
// the fail-closed rule: once the budget (bootretry.Retries: 5 attempts,
// 1+2+4+8 = 15s of backoff) is exhausted this still refuses to boot.
//
// migrationsDatabaseURL is used ONLY for the golang-migrate step below —
// the pgxpool opened just after it (and used for every subsequent
// request) always uses databaseURL. They are deliberately different
// connection strings in a PgBouncer-fronted environment: golang-migrate's
// postgres driver takes a session-scoped `SELECT pg_advisory_lock($1)` to
// serialize concurrent migration runs across replicas starting at the
// same time, and PgBouncer's transaction-pooling mode (this fleet's
// pool_mode for every OLTP DATABASE_URL, warehouse-infra PR #43) does not
// support session-scoped state — each statement in one logical client
// session can land on a different physical backend connection, so the
// advisory lock never behaves as a real mutex. Losing replicas crash-loop
// with `pq: unnamed prepared statement does not exist` / `pq: canceling
// statement due to statement timeout` until one wins the race. See ADR
// 0025-migrations-direct-postgres-connection.md (and order-management's
// ADR-0029, the reference implementation) for the full incident and fix.
// Callers pass MIGRATIONS_DATABASE_URL when set (warehouse-infra now
// provisions it as a direct, non-pooled DSN alongside DATABASE_URL for
// all 9 OLTP services, per its companion PR #44) or fall back to
// databaseURL itself for any environment that doesn't provision the
// split (local dev, CI integration tests) — byte-identical to this
// function's behavior before this parameter existed in that case.
func openPostgresPool(ctx context.Context, logger *slog.Logger, databaseURL, migrationsDatabaseURL, migrationsPath string) (*pgxpool.Pool, error) {
	if err := bootretry.Retry(ctx, logger, "run migrations", func() error {
		return postgres.Migrate(migrationsDatabaseURL, migrationsPath)
	}); err != nil {
		return nil, err
	}
	var pool *pgxpool.Pool
	if err := bootretry.Retry(ctx, logger, "open postgres pool", func() error {
		var err error
		pool, err = postgres.NewPool(ctx, databaseURL)
		return err
	}); err != nil {
		return nil, err
	}
	return pool, nil
}

// newMeasuredRateClients selects the MeasuredRateClient implementation from
// the validated LABOR_PERFORMANCE_MODE (kafka-cache, the default, or http as
// the REST rollback; permissive no longer exists, ADR-0038). "kafka-cache"
// replaces the synchronous HTTP call with
// a local, in-memory read model fed by labor-performance's
// warehouse.labor-performance.events integration topic (ADR 0013 on
// labor-performance's side; see this repo's own ADR for the
// consuming-side rationale) -- mirroring EXACTLY how the process-path
// catalogue starts and waits for
// internal/adapters/outbound/kafkacatalog's consumer: start the Run
// goroutine BEFORE WaitReady is called, so something is always
// consuming while this process waits (otherwise a guaranteed deadlock
// until WaitReadyTimeout).
//
// The SAME kafka-cache Consumer instance also satisfies
// ports.IdleShareClient (idleness-as-staffing-signal): only kafka-cache
// mode has a per-message idle_seconds_before stream to observe, so http
// mode leaves idleShare nil -- GetStaffingGap and ProposePathPlan both
// already treat a nil IdleShareClient as "no signal".
func newMeasuredRateClients(ctx, rateConsumerCtx context.Context, cfg bootConfig, breakerMetrics resilience.StateRecorder, logger *slog.Logger) (ports.MeasuredRateClient, ports.IdleShareClient, *laborperformancecache.Consumer, error) {
	if cfg.laborMode != laborModeKafkaCache {
		return buildMeasuredRateClient(cfg.laborBaseURL, breakerMetrics, logger), nil, nil, nil
	}

	// Retried for the same reason as the process-path catalogue
	// consumer: construction dials Kafka directly to determine its
	// readiness watermark, and that is this fleet's
	// first-outbound-dial-reset condition.
	var kafkaMeasuredRate *laborperformancecache.Consumer
	if err := bootretry.Retry(ctx, logger, "connect labor-performance measured rate cache kafka consumer", func() error {
		var newErr error
		kafkaMeasuredRate, newErr = laborperformancecache.NewConsumer(ctx, cfg.brokers, logger)
		return newErr
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to start the Kafka-sourced labor-performance measured rate cache: %w", err)
	}
	logger.Info("labor-performance measured rate client configured", "mode", "kafka-cache", "topic", laborperformancecache.Topic)
	go func() {
		logger.Info("labor-performance measured rate cache consumer running", "topic", laborperformancecache.Topic)
		if err := kafkaMeasuredRate.Run(rateConsumerCtx); err != nil {
			logger.Error("labor-performance measured rate cache consumer stopped", "error", err)
		}
	}()

	logger.Info("waiting for the labor-performance measured rate cache to replay its initial history before accepting traffic")
	waitCtx, waitCancel := context.WithTimeout(context.Background(), laborperformancecache.WaitReadyTimeout)
	err := kafkaMeasuredRate.WaitReady(waitCtx)
	waitCancel()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("labor-performance measured rate cache did not become ready within %s: %w", laborperformancecache.WaitReadyTimeout, err)
	}
	logger.Info("labor-performance measured rate cache is ready")
	return kafkaMeasuredRate, kafkaMeasuredRate, kafkaMeasuredRate, nil
}

// oltpHandlerDeps groups the inbound handler's wiring inputs.
type oltpHandlerDeps struct {
	associates       *postgres.AssociateRepo
	shiftPlans       *postgres.ShiftPlanRepo
	assignments      *postgres.AssignmentRepo
	publisher        ports.EventPublisher
	sysClock         clock.System
	uow              ports.UnitOfWork
	measuredRate     ports.MeasuredRateClient
	idleShare        ports.IdleShareClient
	breakerMetrics   resilience.StateRecorder
	maxHoursPerShift float64
	catalogue        ports.PathCatalogue
	readiness        *inbound.Readiness
	idempotencyPool  *pgxpool.Pool
	logger           *slog.Logger
}

// newOLTPHandler wires every use case into the inbound handler.
func newOLTPHandler(d oltpHandlerDeps) *inbound.Handler {
	installedCapacity := buildInstalledCapacityClient(envOrDefault("INSTALLED_CAPACITY_MODE", "permissive"), os.Getenv("FULFILLMENT_EXECUTION_BASE_URL"), d.breakerMetrics, d.logger)
	idleShareTrimThreshold := envFloatOrDefault("IDLE_SHARE_TRIM_THRESHOLD", usecases.DefaultIdleShareTrimThreshold)
	return &inbound.Handler{
		StartAssociateShift: &usecases.StartAssociateShift{Associates: d.associates, Events: d.publisher, Clock: d.sysClock, UnitOfWork: d.uow},
		CertifyAssociate:    &usecases.CertifyAssociate{Associates: d.associates, Events: d.publisher, Clock: d.sysClock, UnitOfWork: d.uow},
		ProposePathPlan:     &usecases.ProposePathPlan{Events: d.publisher, Clock: d.sysClock, MeasuredRate: d.measuredRate, IdleShare: d.idleShare, IdleShareTrimThreshold: idleShareTrimThreshold, UnitOfWork: d.uow},
		CommitShiftPlan:     &usecases.CommitShiftPlan{ShiftPlans: d.shiftPlans, Events: d.publisher, Clock: d.sysClock, InstalledCapacity: installedCapacity, Catalogue: d.catalogue, MaxHoursPerShift: d.maxHoursPerShift, UnitOfWork: d.uow},
		AssignLabor:         &usecases.AssignLabor{Associates: d.associates, Assignments: d.assignments, Events: d.publisher, Clock: d.sysClock, MaxHoursPerShift: d.maxHoursPerShift, Catalogue: d.catalogue, Metrics: newLaborMetrics(d.logger), UnitOfWork: d.uow},
		StartBreak:          &usecases.StartBreak{Associates: d.associates, Events: d.publisher, Clock: d.sysClock, UnitOfWork: d.uow},
		EndBreak:            &usecases.EndBreak{Associates: d.associates, Events: d.publisher, Clock: d.sysClock, UnitOfWork: d.uow},
		GetStaffingGap:      &usecases.GetStaffingGap{ShiftPlans: d.shiftPlans, Assignments: d.assignments, Events: d.publisher, Clock: d.sysClock, IdleShare: d.idleShare, UnitOfWork: d.uow},
		EndAssociateShift:   &usecases.EndAssociateShift{Associates: d.associates, Assignments: d.assignments, Events: d.publisher, Clock: d.sysClock, MaxHoursPerShift: d.maxHoursPerShift, UnitOfWork: d.uow},
		Catalogue:           d.catalogue,
		// readiness backs GET /readyz (ADR-0022 §graceful shutdown):
		// flipped to not-ready as the FIRST step of shutdown, below,
		// before anything else stops.
		Readiness: d.readiness,
		// IdempotencyPool wires RequireIdempotencyKey onto the creation POSTs (ADR-0027).
		IdempotencyPool: d.idempotencyPool,
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// buildMeasuredRateClient builds the http-mode ports.MeasuredRateClient (the
// REST rollback of the kafka-cache default, R10): the real Client wrapped in
// a per-dependency circuit breaker plus jittered retry (ADR-0022, ported from
// order-management's ADR-0025): MeanActualSeconds is a pure GET/read, safe to
// retry unlike fulfillment-execution's commit-gating InstalledCapacity below.
// On a trip, calls fall back to the permissive fail-open behaviour inside the
// breaker, never a new fallback path. recorder feeds the breaker's state
// transitions into the circuit_breaker.state gauge; nil is fine (see
// resilience.RecordStateChange's doc comment). baseURL is validated
// non-empty at boot (loadBootConfig).
func buildMeasuredRateClient(baseURL string, recorder resilience.StateRecorder, logger *slog.Logger) ports.MeasuredRateClient {
	logger.Info("labor-performance measured rate client configured", "mode", "http", "base_url", baseURL, "circuit_breaker", "enabled", "retry", "enabled")
	return laborperformance.NewBreakerClient(laborperformance.NewClient(baseURL, nil), recorder)
}

// buildInstalledCapacityClient selects a ports.InstalledCapacityClient via
// mode (http|permissive), defaulting to "permissive" -- unlike
// buildMeasuredRateClient's fail-open default, the permissive mode here
// makes CommitShiftPlan fail EVERY commit until INSTALLED_CAPACITY_MODE=http
// is explicitly set, since a shift-plan commit mutates real state and
// this fleet's own rule is to fail loud for anything that mutates real
// state. See ADR-0014.
//
// In http mode the real Client is wrapped in a per-dependency circuit
// breaker (ADR-0022) with NO retry -- this call gates a COMMIT that
// mutates real state, so (mirroring order-management's
// inventorystorage.BreakerClient) it deliberately never blind-retries;
// see fulfillmentexecution/breaker.go's doc comment for the full
// rationale. On a trip, calls fall back to the SAME fail-LOUD permissive
// behaviour this client already had. recorder feeds the breaker's state
// transitions into the circuit_breaker.state gauge; nil is fine.
func buildInstalledCapacityClient(mode, baseURL string, recorder resilience.StateRecorder, logger *slog.Logger) ports.InstalledCapacityClient {
	if mode != "http" {
		logger.Warn("fulfillment-execution installed capacity client configured", "mode", "permissive",
			"hint", "every ShiftPlan commit will fail until INSTALLED_CAPACITY_MODE=http and FULFILLMENT_EXECUTION_BASE_URL are set for a real deployment")
		return fulfillmentexecution.NewPermissiveClient()
	}
	logger.Info("fulfillment-execution installed capacity client configured", "mode", "http", "base_url", baseURL, "circuit_breaker", "enabled")
	return fulfillmentexecution.NewBreakerClient(fulfillmentexecution.NewClient(baseURL, nil), recorder)
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

// envNonNegativeIntOrDefault parses key as a non-negative integer, falling
// back to def when it is unset and, with a warning, when it is malformed or
// negative: like the relay interval it is a tuning knob, so it never fails
// the boot. 0 is a valid value (OUTBOX_MAX_ATTEMPTS uses it for
// "unlimited").
func envNonNegativeIntOrDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		slog.Warn("invalid integer env var, using default", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

// envDurationOrDefault parses key as a time.Duration, falling back on
// absence or a malformed/non-positive value: the relay interval is a tuning
// knob, not a contract, so it never fails the boot.
func envDurationOrDefault(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("invalid duration env var, using default", "key", key, "value", v, "default", def)
		return def
	}
	return d
}
