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
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inbound "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/clock"
	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/kafkacatalog"
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
	defer flushTelemetry(logger, shutdownTelemetry)

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
