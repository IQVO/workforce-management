// Command workforce is the composition root for the Workforce Management
// service: it wires config from the environment to adapters, use cases, and
// the HTTP router, then serves.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inbound "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/bootretry"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/clock"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/fulfillmentexecution"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
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
	if err := run(); err != nil {
		slog.Error("service exited with error", "error", err)
		os.Exit(1)
	}
}

// newLogger builds the service's JSON-to-stdout slog.Logger. level accepts
// debug|info|warn|error (case-insensitive), defaulting to info for an
// unrecognized value.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
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

func run() error {
	logger := newLogger(envOrDefault("LOG_LEVEL", "info"))
	slog.SetDefault(logger)

	ctx := context.Background()

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

	// readiness gates GET /readyz (ADR-0022 §graceful shutdown, ported
	// from order-management's ADR-0025): zero value ready, flipped
	// not-ready as the FIRST step of the shutdown sequence below.
	readiness := &inbound.Readiness{}

	breakerMetrics := newBreakerMetrics(logger)

	databaseURL := requireEnv("DATABASE_URL")
	// MIGRATIONS_DATABASE_URL, when set, is a DIRECT (non-pooled,
	// session-mode) Postgres connection string used ONLY for the
	// golang-migrate startup step below — everything else (the pgxpool
	// this process serves requests through) keeps using databaseURL
	// unchanged. See openPostgresPool's doc comment for the full "why":
	// golang-migrate's postgres driver takes a session-scoped
	// `SELECT pg_advisory_lock($1)` to serialize concurrent migration
	// runs, which PgBouncer's transaction-pooling mode does not support
	// (warehouse-infra's PgBouncer rollout, PR #43; this fallback closes
	// the fleet-wide bug that rollout introduced — see ADR
	// 0025-migrations-direct-postgres-connection.md and
	// order-management's ADR-0029, the reference implementation this PR
	// ports). Falls back to databaseURL when unset, which is every
	// environment that doesn't provision the split (local dev, CI
	// integration tests, and any cluster whose Terraform predates this
	// fix) — byte-identical to this service's behavior before this
	// change in that case.
	migrationsDatabaseURL := envOrDefault("MIGRATIONS_DATABASE_URL", databaseURL)
	httpAddr := envOrDefault("HTTP_ADDR", ":8080")
	migrationsPath := envOrDefault("MIGRATIONS_PATH", "migrations")
	maxHoursPerShift := envFloatOrDefault("MAX_HOURS_PER_SHIFT", 8.0)

	// catalogueConsumerCtx/cancelCatalogueConsumer are declared here
	// (rather than deferred to later in run()) because the Kafka
	// catalogue source needs its own Run goroutine started BEFORE
	// WaitReady is called below -- otherwise nothing would ever be
	// consuming messages while this process waits, guaranteeing a
	// deadlock until WaitReadyTimeout.
	catalogueConsumerCtx, cancelCatalogueConsumer := context.WithCancel(context.Background())
	defer cancelCatalogueConsumer()

	catalogue, kafkaCatalogue, err := composition.BuildCatalogue(ctx, catalogueConsumerCtx, catalogueConfigFromEnv(), logger)
	if err != nil {
		return err
	}

	pool, err := openPostgresPool(ctx, logger, databaseURL, migrationsDatabaseURL, migrationsPath)
	if err != nil {
		return err
	}
	defer pool.Close()

	repos, closeRepos, err := wireRepos(pool, logger)
	if err != nil {
		return err
	}
	defer closeRepos()
	sysClock := clock.System{}

	rateConsumerCtx, cancelRateConsumer := context.WithCancel(context.Background())
	defer cancelRateConsumer()
	measuredRate, idleShare, kafkaMeasuredRate, err := newMeasuredRateClients(ctx, rateConsumerCtx, breakerMetrics, logger)
	if err != nil {
		return err
	}
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
		maxHoursPerShift: maxHoursPerShift,
		catalogue:        catalogue,
		readiness:        readiness,
		logger:           logger,
	})

	server := &http.Server{
		Addr:              httpAddr,
		Handler:           inbound.NewRouter(handler, logger, serviceName),
		ReadHeaderTimeout: 5 * time.Second,
	}

	return serveWorkforce(logger, httpAddr, server, readiness, repos.relay,
		cancelCatalogueConsumer, kafkaCatalogue,
		cancelRateConsumer, kafkaMeasuredRate)
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

// wireRepos builds the Postgres repositories, the event publisher (log or
// kafka-outbox per EVENT_PUBLISHER), and the shared UnitOfWork. The
// returned close func releases the publisher adapters; the caller defers
// it so teardown stays LIFO with the pool close. Every publishing use
// case shares one UnitOfWork so its Saves and its Publish (an outbox
// INSERT in kafka mode) commit together (ADR 0016). The log publisher
// has nothing to bind, but bracketing the Saves in a transaction is
// still correct, so the UnitOfWork is wired unconditionally.
func wireRepos(pool *pgxpool.Pool, logger *slog.Logger) (*workforceRepos, func(), error) {
	shiftPlans := postgres.NewShiftPlanRepo(pool)
	built, err := composition.BuildEventPublisher(publisherConfigFromEnv(), pool, shiftPlans, logger)
	if err != nil {
		return nil, nil, err
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

// publisherConfigFromEnv resolves EVENT_PUBLISHER, KAFKA_BROKERS and
// OUTBOX_RELAY_INTERVAL for the shared composition.BuildEventPublisher.
// This process owns the outbox relay (RunRelay): cmd/mcp only inserts.
func publisherConfigFromEnv() composition.PublisherConfig {
	return composition.PublisherConfig{
		Kind:          envOrDefault("EVENT_PUBLISHER", composition.LogPublisher),
		Brokers:       strings.Split(envOrDefault("KAFKA_BROKERS", "localhost:9092"), ","),
		RunRelay:      true,
		RelayInterval: envDurationOrDefault("OUTBOX_RELAY_INTERVAL", time.Second),
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

// catalogueConfigFromEnv resolves PATH_CATALOGUE_SOURCE (file|kafka,
// default file), PATH_CATALOGUE_FILE and KAFKA_BROKERS for the shared
// composition.BuildCatalogue, which cmd/mcp also uses so both surfaces
// validate path ids against the same catalogue (ADR-0013).
func catalogueConfigFromEnv() composition.CatalogueConfig {
	return composition.CatalogueConfig{
		Source:  envOrDefault("PATH_CATALOGUE_SOURCE", composition.CatalogueFromFile),
		File:    envOrDefault("PATH_CATALOGUE_FILE", "/etc/workforce-management/process-paths.yaml"),
		Brokers: strings.Split(os.Getenv("KAFKA_BROKERS"), ","),
	}
}

// openPostgresPool runs the schema migrations, opens the pool, each
// under boot retry: in this fleet EVERY injected pod's FIRST outbound
// TCP dial (here, Postgres) is reset ~10s after the app starts (Istio
// native sidecars). A single attempt turns that known, transient
// condition into CrashLoopBackOff — migrations fail with "read:
// connection reset by peer", the process exits, and the pod never gets
// far enough to serve its own health probe. The retry does not weaken
// the fail-closed rule: once the budget (bootretry.Retries, ~31s total)
// is exhausted this still refuses to boot.
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

// newMeasuredRateClients selects the MeasuredRateClient implementation
// via LABOR_PERFORMANCE_MODE (http|kafka-cache|permissive, default
// "permissive"). "kafka-cache" replaces the synchronous HTTP call with
// a local, in-memory read model fed by labor-performance's
// warehouse.labor-performance.events integration topic (ADR 0013 on
// labor-performance's side; see this repo's own ADR for the
// consuming-side rationale) -- mirroring EXACTLY how
// PATH_CATALOGUE_SOURCE=kafka starts and waits for
// internal/adapters/outbound/kafkacatalog's consumer: start the Run
// goroutine BEFORE WaitReady is called, so something is always
// consuming while this process waits (otherwise a guaranteed deadlock
// until WaitReadyTimeout).
//
// The SAME kafka-cache Consumer instance also satisfies
// ports.IdleShareClient (idleness-as-staffing-signal): only kafka-cache
// mode has a per-message idle_seconds_before stream to observe, so http
// and permissive leave idleShare nil -- GetStaffingGap and
// ProposePathPlan both already treat a nil IdleShareClient as "no
// signal", the same fail-open discipline as every other *_MODE default
// in this fleet.
func newMeasuredRateClients(ctx, rateConsumerCtx context.Context, breakerMetrics resilience.StateRecorder, logger *slog.Logger) (ports.MeasuredRateClient, ports.IdleShareClient, *laborperformancecache.Consumer, error) {
	mode := envOrDefault("LABOR_PERFORMANCE_MODE", "permissive")
	if mode != "kafka-cache" {
		return buildMeasuredRateClient(mode, os.Getenv("LABOR_PERFORMANCE_BASE_URL"), breakerMetrics, logger), nil, nil, nil
	}

	kafkaBrokersCSV := os.Getenv("KAFKA_BROKERS")
	if kafkaBrokersCSV == "" {
		return nil, nil, nil, fmt.Errorf("LABOR_PERFORMANCE_MODE=kafka-cache requires KAFKA_BROKERS to be set")
	}
	// Retried for the same reason as the process-path catalogue
	// consumer: construction dials Kafka directly to determine its
	// readiness watermark, and that is this fleet's
	// first-outbound-dial-reset condition.
	var kafkaMeasuredRate *laborperformancecache.Consumer
	if err := bootretry.Retry(ctx, logger, "connect labor-performance measured rate cache kafka consumer", func() error {
		var newErr error
		kafkaMeasuredRate, newErr = laborperformancecache.NewConsumer(ctx, strings.Split(kafkaBrokersCSV, ","), logger)
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
		AssignLabor:         &usecases.AssignLabor{Associates: d.associates, Assignments: d.assignments, Events: d.publisher, Clock: d.sysClock, MaxHoursPerShift: d.maxHoursPerShift, UnitOfWork: d.uow},
		StartBreak:          &usecases.StartBreak{Associates: d.associates, Events: d.publisher, Clock: d.sysClock, UnitOfWork: d.uow},
		EndBreak:            &usecases.EndBreak{Associates: d.associates, Events: d.publisher, Clock: d.sysClock, UnitOfWork: d.uow},
		GetStaffingGap:      &usecases.GetStaffingGap{ShiftPlans: d.shiftPlans, Assignments: d.assignments, Events: d.publisher, Clock: d.sysClock, IdleShare: d.idleShare, UnitOfWork: d.uow},
		EndAssociateShift:   &usecases.EndAssociateShift{Associates: d.associates, Assignments: d.assignments, Events: d.publisher, Clock: d.sysClock, MaxHoursPerShift: d.maxHoursPerShift, UnitOfWork: d.uow},
		Catalogue:           d.catalogue,
		// readiness backs GET /readyz (ADR-0022 §graceful shutdown):
		// flipped to not-ready as the FIRST step of shutdown, below,
		// before anything else stops.
		Readiness: d.readiness,
	}
}

// serveWorkforce runs the HTTP server until a signal arrives or the
// listener fails, with the outbox relay draining alongside, then drains
// everything under the ADR-0022 §graceful shutdown sequence.
func serveWorkforce(logger *slog.Logger, httpAddr string, server *http.Server, readiness *inbound.Readiness, relay *postgres.OutboxRelay,
	cancelCatalogueConsumer context.CancelFunc, kafkaCatalogue *kafkacatalog.Consumer,
	cancelRateConsumer context.CancelFunc, kafkaMeasuredRate *laborperformancecache.Consumer) error {
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", httpAddr)
		serverErr <- server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// The outbox relay (ADR 0016) runs alongside the HTTP server in the
	// same process, draining outbox_events onto both Kafka topics. It is
	// only wired in kafka mode (see newEventPublisher).
	relayDone := make(chan struct{})
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	if relay != nil {
		go func() {
			defer close(relayDone)
			logger.Info("outbox relay running", "topics", []string{kafka.Topic, kafka.AnalyticsTopic})
			if err := relay.Run(relayCtx); err != nil && !errors.Is(err, context.Canceled) {
				serverErr <- err
			}
		}()
	} else {
		close(relayDone)
	}

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-stop:
		// Graceful shutdown (ADR-0022 §graceful shutdown, ported from
		// order-management's ADR-0025): flip readiness to not-ready
		// FIRST, before anything else stops -- a Kubernetes
		// readinessProbe polling /readyz needs a window to observe
		// this and stop routing NEW traffic to this pod before the
		// HTTP listener itself closes below.
		readiness.SetNotReady()

		cancelCatalogueConsumer()
		if kafkaCatalogue != nil {
			_ = kafkaCatalogue.Close()
		}
		cancelRateConsumer()
		if kafkaMeasuredRate != nil {
			_ = kafkaMeasuredRate.Close()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := server.Shutdown(shutdownCtx)
		// Let the relay finish its in-flight pass so an event committed by
		// a request that completed just before shutdown is not stranded
		// until the next pod boots.
		stopRelay()
		select {
		case <-relayDone:
		case <-shutdownCtx.Done():
			logger.Warn("outbox relay did not stop before the shutdown deadline")
		}
		return err
	}
	return nil
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required env var", "key", key)
		os.Exit(1)
	}
	return v
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// buildMeasuredRateClient selects a ports.MeasuredRateClient via mode
// (http|permissive), defaulting to "permissive" so unit tests, local dev,
// and CI never reach the network unless explicitly opted in -- the same
// pattern order-management uses for INVENTORY_STORAGE_MODE. A third mode,
// "kafka-cache", is handled separately in run() (mirroring
// PATH_CATALOGUE_SOURCE=kafka's wiring) because it needs a consumer
// goroutine and a WaitReady gate before this service is ready to serve
// traffic -- this function stays scoped to the two modes that construct
// synchronously with no startup ordering to manage.
//
// In http mode the real Client is wrapped in a per-dependency circuit
// breaker plus jittered retry (ADR-0022, ported from order-management's
// ADR-0025): MeanActualSeconds is a pure GET/read, safe to retry unlike
// fulfillment-execution's commit-gating InstalledCapacity below. On a
// trip, calls fall back to the SAME fail-open permissive behaviour this
// client already had, never a new fallback path. recorder feeds the
// breaker's state transitions into the circuit_breaker.state gauge; nil
// is fine (see resilience.RecordStateChange's doc comment).
func buildMeasuredRateClient(mode, baseURL string, recorder resilience.StateRecorder, logger *slog.Logger) ports.MeasuredRateClient {
	if mode != "http" {
		logger.Info("labor-performance measured rate client configured", "mode", "permissive",
			"hint", "set LABOR_PERFORMANCE_MODE=http and LABOR_PERFORMANCE_BASE_URL for a real deployment")
		return laborperformance.NewPermissiveClient()
	}
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
