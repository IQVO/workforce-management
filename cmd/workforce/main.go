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
	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/filecatalog"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/fulfillmentexecution"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/kafkacatalog"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/laborperformance"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/laborperformancecache"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/telemetry"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
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

	serviceName := envOrDefault("OTEL_SERVICE_NAME", inbound.DefaultServiceName)
	otlpEndpoint := envOrDefault("OTEL_EXPORTER_OTLP_ENDPOINT", telemetry.DefaultOTLPEndpoint)
	shutdownTelemetry, err := telemetry.Setup(ctx, serviceName, serviceVersion(), otlpEndpoint)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Warn, not Error: the usual cause is "no Collector listening at
		// OTEL_EXPORTER_OTLP_ENDPOINT", which drops the final flush but is
		// not a service failure.
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			logger.Warn("telemetry shutdown did not flush cleanly", "error", err)
		}
	}()
	logger.Info("telemetry configured",
		"service_name", serviceName,
		"service_version", serviceVersion(),
		"environment", telemetry.Environment(),
		"otlp_endpoint", otlpEndpoint,
	)

	databaseURL := requireEnv("DATABASE_URL")
	httpAddr := envOrDefault("HTTP_ADDR", ":8080")
	migrationsPath := envOrDefault("MIGRATIONS_PATH", "migrations")
	maxHoursPerShift := envFloatOrDefault("MAX_HOURS_PER_SHIFT", 8.0)

	// The process-path catalogue's SOURCE is selectable, defaulting to
	// the existing boot-time file read ("file") -- zero behavior change
	// for any existing deployment unless PATH_CATALOGUE_SOURCE=kafka is
	// explicitly set, matching this fleet's EVENT_PUBLISHER convention.
	// See internal/adapters/outbound/kafkacatalog's package doc comment
	// for the full rationale and the readiness-gate design, mirrored
	// byte-for-byte from fulfillment-execution's and
	// wes-work-planning's identical wiring.
	catalogueSource := envOrDefault("PATH_CATALOGUE_SOURCE", "file")

	var catalogue ports.PathCatalogue
	var kafkaCatalogue *kafkacatalog.Consumer
	// catalogueConsumerCtx/cancelCatalogueConsumer are declared here
	// (rather than deferred to later in run()) because the Kafka
	// catalogue source needs its own Run goroutine started BEFORE
	// WaitReady is called below -- otherwise nothing would ever be
	// consuming messages while this process waits, guaranteeing a
	// deadlock until WaitReadyTimeout.
	catalogueConsumerCtx, cancelCatalogueConsumer := context.WithCancel(context.Background())
	defer cancelCatalogueConsumer()

	switch catalogueSource {
	case "kafka":
		kafkaBrokersCSV := os.Getenv("KAFKA_BROKERS")
		if kafkaBrokersCSV == "" {
			return fmt.Errorf("PATH_CATALOGUE_SOURCE=kafka requires KAFKA_BROKERS to be set")
		}
		// Retried: this consumer's construction dials Kafka directly to
		// determine its readiness watermark (newTargetOffsets), and in
		// this fleet EVERY injected pod's first outbound dial is reset
		// ~10s after start (Istio native sidecars). One attempt here
		// turns that transient into the same CrashLoopBackOff the
		// Postgres boot dial below is guarded against.
		var err error
		if err = bootretry.Retry(ctx, logger, "connect process-path catalogue kafka consumer", func() error {
			var newErr error
			kafkaCatalogue, newErr = kafkacatalog.NewConsumer(ctx, strings.Split(kafkaBrokersCSV, ","), logger)
			return newErr
		}); err != nil {
			return fmt.Errorf("failed to start the Kafka-sourced process-path catalogue: %w", err)
		}
		logger.Info("process-path catalogue source configured", "source", "kafka", "topic", kafkacatalog.Topic)
		go func() {
			logger.Info("process-path catalogue consumer running", "topic", kafkacatalog.Topic)
			if err := kafkaCatalogue.Run(catalogueConsumerCtx); err != nil {
				logger.Error("process-path catalogue consumer stopped", "error", err)
			}
		}()

		logger.Info("waiting for the process-path catalogue to replay its initial history before accepting traffic")
		waitCtx, waitCancel := context.WithTimeout(context.Background(), kafkacatalog.WaitReadyTimeout)
		err = kafkaCatalogue.WaitReady(waitCtx)
		waitCancel()
		if err != nil {
			return fmt.Errorf("process-path catalogue did not become ready within %s: %w", kafkacatalog.WaitReadyTimeout, err)
		}
		logger.Info("process-path catalogue is ready", "paths", kafkaCatalogue.Ids())
		catalogue = kafkaCatalogue
	default:
		// The process-path catalogue is loaded and validated once at
		// boot, before anything else stands up — a missing or
		// malformed catalogue file must stop this service from
		// starting at all, never fall back to a partial/empty
		// catalogue (mirrors fulfillment-execution's and
		// wes-work-planning's identical boot-time contract; see
		// ADR-0013).
		fileCatalogue, err := filecatalog.Load(envOrDefault("PATH_CATALOGUE_FILE", "/etc/workforce-management/process-paths.yaml"))
		if err != nil {
			return fmt.Errorf("failed to load the process-path catalogue: %w", err)
		}
		logger.Info("process-path catalogue loaded", "paths", fileCatalogue.Ids())
		catalogue = fileCatalogue
	}

	// Retried: in this fleet EVERY injected pod's FIRST outbound TCP dial
	// (here, Postgres) is reset ~10s after the app starts (Istio native
	// sidecars). A single attempt turns that known, transient condition
	// into CrashLoopBackOff — migrations fail with "read: connection
	// reset by peer", the process exits, and the pod never gets far
	// enough to serve its own health probe. The retry does not weaken
	// the fail-closed rule: once the budget (bootretry.Retries,
	// ~31s total) is exhausted this still refuses to boot.
	if err := bootretry.Retry(ctx, logger, "run migrations", func() error {
		return postgres.Migrate(databaseURL, migrationsPath)
	}); err != nil {
		return err
	}

	var pool *pgxpool.Pool
	if err := bootretry.Retry(ctx, logger, "open postgres pool", func() error {
		var err error
		pool, err = postgres.NewPool(ctx, databaseURL)
		return err
	}); err != nil {
		return err
	}
	defer pool.Close()

	associates := postgres.NewAssociateRepo(pool)
	shiftPlans := postgres.NewShiftPlanRepo(pool)
	assignments := postgres.NewAssignmentRepo(pool)
	sysClock := clock.System{}

	publisher, relay, closePublisher, err := newEventPublisher(pool, shiftPlans, logger)
	if err != nil {
		return err
	}
	defer closePublisher()
	// Every publishing use case shares one UnitOfWork so its Saves and its
	// Publish (an outbox INSERT in kafka mode) commit together (ADR 0016).
	// The log publisher has nothing to bind, but bracketing the Saves in a
	// transaction is still correct, so the UnitOfWork is wired unconditionally.
	uow := postgres.NewUnitOfWork(pool)

	// LABOR_PERFORMANCE_MODE selects the MeasuredRateClient
	// implementation (http|kafka-cache|permissive, default
	// "permissive"). "kafka-cache" replaces the synchronous HTTP call
	// with a local, in-memory read model fed by labor-performance's
	// warehouse.labor-performance.events integration topic (ADR 0013 on
	// labor-performance's side; see this repo's own ADR for the
	// consuming-side rationale) -- mirroring EXACTLY how
	// PATH_CATALOGUE_SOURCE=kafka starts and waits for
	// internal/adapters/outbound/kafkacatalog's consumer above: start
	// the Run goroutine BEFORE WaitReady is called, so something is
	// always consuming while this process waits (otherwise a guaranteed
	// deadlock until WaitReadyTimeout).
	var measuredRate ports.MeasuredRateClient
	var idleShare ports.IdleShareClient
	var kafkaMeasuredRate *laborperformancecache.Consumer
	rateConsumerCtx, cancelRateConsumer := context.WithCancel(context.Background())
	defer cancelRateConsumer()

	switch envOrDefault("LABOR_PERFORMANCE_MODE", "permissive") {
	case "kafka-cache":
		kafkaBrokersCSV := os.Getenv("KAFKA_BROKERS")
		if kafkaBrokersCSV == "" {
			return fmt.Errorf("LABOR_PERFORMANCE_MODE=kafka-cache requires KAFKA_BROKERS to be set")
		}
		// Retried for the same reason as the process-path catalogue
		// consumer above: construction dials Kafka directly to
		// determine its readiness watermark, and that is this fleet's
		// first-outbound-dial-reset condition.
		var err error
		if err = bootretry.Retry(ctx, logger, "connect labor-performance measured rate cache kafka consumer", func() error {
			var newErr error
			kafkaMeasuredRate, newErr = laborperformancecache.NewConsumer(ctx, strings.Split(kafkaBrokersCSV, ","), logger)
			return newErr
		}); err != nil {
			return fmt.Errorf("failed to start the Kafka-sourced labor-performance measured rate cache: %w", err)
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
		err = kafkaMeasuredRate.WaitReady(waitCtx)
		waitCancel()
		if err != nil {
			return fmt.Errorf("labor-performance measured rate cache did not become ready within %s: %w", laborperformancecache.WaitReadyTimeout, err)
		}
		logger.Info("labor-performance measured rate cache is ready")
		measuredRate = kafkaMeasuredRate
		// The SAME Consumer instance also satisfies ports.IdleShareClient
		// (idleness-as-staffing-signal): only kafka-cache mode has a
		// per-message idle_seconds_before stream to observe, so http and
		// permissive leave idleShare nil below -- GetStaffingGap and
		// ProposePathPlan both already treat a nil IdleShareClient as
		// "no signal", the same fail-open discipline as every other
		// *_MODE default in this fleet.
		idleShare = kafkaMeasuredRate
	default:
		measuredRate = buildMeasuredRateClient(envOrDefault("LABOR_PERFORMANCE_MODE", "permissive"), os.Getenv("LABOR_PERFORMANCE_BASE_URL"), logger)
	}
	installedCapacity := buildInstalledCapacityClient(envOrDefault("INSTALLED_CAPACITY_MODE", "permissive"), os.Getenv("FULFILLMENT_EXECUTION_BASE_URL"), logger)
	idleShareTrimThreshold := envFloatOrDefault("IDLE_SHARE_TRIM_THRESHOLD", usecases.DefaultIdleShareTrimThreshold)

	handler := &inbound.Handler{
		StartAssociateShift: &usecases.StartAssociateShift{Associates: associates, Events: publisher, Clock: sysClock, UnitOfWork: uow},
		CertifyAssociate:    &usecases.CertifyAssociate{Associates: associates, Events: publisher, Clock: sysClock, UnitOfWork: uow},
		ProposePathPlan:     &usecases.ProposePathPlan{Events: publisher, Clock: sysClock, MeasuredRate: measuredRate, IdleShare: idleShare, IdleShareTrimThreshold: idleShareTrimThreshold, UnitOfWork: uow},
		CommitShiftPlan:     &usecases.CommitShiftPlan{ShiftPlans: shiftPlans, Events: publisher, Clock: sysClock, InstalledCapacity: installedCapacity, MaxHoursPerShift: maxHoursPerShift, UnitOfWork: uow},
		AssignLabor:         &usecases.AssignLabor{Associates: associates, Assignments: assignments, Events: publisher, Clock: sysClock, MaxHoursPerShift: maxHoursPerShift, UnitOfWork: uow},
		StartBreak:          &usecases.StartBreak{Associates: associates, Events: publisher, Clock: sysClock, UnitOfWork: uow},
		EndBreak:            &usecases.EndBreak{Associates: associates, Events: publisher, Clock: sysClock, UnitOfWork: uow},
		GetStaffingGap:      &usecases.GetStaffingGap{ShiftPlans: shiftPlans, Assignments: assignments, Events: publisher, Clock: sysClock, IdleShare: idleShare, UnitOfWork: uow},
		EndAssociateShift:   &usecases.EndAssociateShift{Associates: associates, Assignments: assignments, Events: publisher, Clock: sysClock, MaxHoursPerShift: maxHoursPerShift, UnitOfWork: uow},
		Catalogue:           catalogue,
	}

	server := &http.Server{
		Addr:              httpAddr,
		Handler:           inbound.NewRouter(handler, logger, serviceName),
		ReadHeaderTimeout: 5 * time.Second,
	}

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

// newEventPublisher selects an EventPublisher via EVENT_PUBLISHER
// (kafka|log, default log) so existing behavior — and existing tests — are
// unaffected unless kafka is explicitly opted into. It returns the
// publisher, the outbox relay to run alongside the HTTP server (nil when
// there is none), and a close func to release adapter resources on
// shutdown.
//
// In kafka mode the use cases publish into the transactional outbox
// (ADR 0016): both Kafka publishers act as Encoders feeding one
// OutboxPublisher, and the relay forwards each row to the topic it names.
// The direct MultiPublisher path is kept only for a nil pool, which this
// binary never has (DATABASE_URL is required) — it is what an in-memory
// composition would use, and it documents the matrix in the ADR.
func newEventPublisher(pool *pgxpool.Pool, shiftPlans ports.ShiftPlanRepo, logger *slog.Logger) (ports.EventPublisher, *postgres.OutboxRelay, func(), error) {
	switch envOrDefault("EVENT_PUBLISHER", "log") {
	case "kafka":
		brokers := strings.Split(envOrDefault("KAFKA_BROKERS", "localhost:9092"), ",")
		integration := kafka.NewPublisher(brokers, shiftPlans)
		// Fan-out: the same domain events also feed the analytics data product
		// on a SEPARATE topic (ADR-0010). The integration publisher/topic is
		// untouched; the analytics publisher is an additive second sink.
		analytics := kafka.NewAnalyticsPublisher(brokers, kafka.NewEventID)
		closeDirect := func() {
			if err := integration.Close(); err != nil {
				logger.Error("kafka publisher close failed", "error", err)
			}
			if err := analytics.Close(); err != nil {
				logger.Error("kafka analytics publisher close failed", "error", err)
			}
		}

		if pool == nil {
			logger.Info("event publisher configured", "publisher", "kafka", "mode", "direct",
				"brokers", brokers, "topic", kafka.Topic, "analytics_topic", kafka.AnalyticsTopic)
			return events.NewMultiPublisher(integration, analytics), nil, closeDirect, nil
		}

		sink := kafka.NewRelaySink(brokers)
		relay := postgres.NewOutboxRelay(pool, sink, logger,
			postgres.WithInterval(envDurationOrDefault("OUTBOX_RELAY_INTERVAL", time.Second)))
		// workforce.outbox.lag_seconds (ADR 0016's flagged follow-up):
		// only meaningful when the outbox is actually the publish path,
		// so it is registered here alongside the relay, not unconditionally.
		lagGaugeReg, err := postgres.RegisterOutboxLagGauge(pool)
		if err != nil {
			logger.Error("failed to register outbox lag gauge", "error", err)
		}
		logger.Info("event publisher configured", "publisher", "kafka", "mode", "outbox",
			"brokers", brokers, "topic", kafka.Topic, "analytics_topic", kafka.AnalyticsTopic)
		return postgres.NewOutboxPublisher(pool, integration, analytics), relay, func() {
			closeDirect()
			if err := sink.Close(); err != nil {
				logger.Error("kafka relay sink close failed", "error", err)
			}
			if lagGaugeReg != nil {
				if err := lagGaugeReg.Unregister(); err != nil {
					logger.Warn("outbox lag gauge unregister failed", "error", err)
				}
			}
		}, nil
	case "log":
		logger.Info("event publisher configured", "publisher", "log")
		return events.NewLogPublisher(logger), nil, func() {}, nil
	default:
		return nil, nil, nil, fmt.Errorf("unknown EVENT_PUBLISHER %q (want kafka or log)", os.Getenv("EVENT_PUBLISHER"))
	}
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
func buildMeasuredRateClient(mode, baseURL string, logger *slog.Logger) ports.MeasuredRateClient {
	if mode != "http" {
		logger.Info("labor-performance measured rate client configured", "mode", "permissive",
			"hint", "set LABOR_PERFORMANCE_MODE=http and LABOR_PERFORMANCE_BASE_URL for a real deployment")
		return laborperformance.NewPermissiveClient()
	}
	logger.Info("labor-performance measured rate client configured", "mode", "http", "base_url", baseURL)
	return laborperformance.NewClient(baseURL, nil)
}

// buildInstalledCapacityClient selects a ports.InstalledCapacityClient via
// mode (http|permissive), defaulting to "permissive" -- unlike
// buildMeasuredRateClient's fail-open default, the permissive mode here
// makes CommitShiftPlan fail EVERY commit until INSTALLED_CAPACITY_MODE=http
// is explicitly set, since a shift-plan commit mutates real state and
// this fleet's own rule is to fail loud for anything that mutates real
// state. See ADR-0014.
func buildInstalledCapacityClient(mode, baseURL string, logger *slog.Logger) ports.InstalledCapacityClient {
	if mode != "http" {
		logger.Warn("fulfillment-execution installed capacity client configured", "mode", "permissive",
			"hint", "every ShiftPlan commit will fail until INSTALLED_CAPACITY_MODE=http and FULFILLMENT_EXECUTION_BASE_URL are set for a real deployment")
		return fulfillmentexecution.NewPermissiveClient()
	}
	logger.Info("fulfillment-execution installed capacity client configured", "mode", "http", "base_url", baseURL)
	return fulfillmentexecution.NewClient(baseURL, nil)
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
