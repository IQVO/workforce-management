// Command workforce-projector is the WRITER composition root of the workforce
// Labor Utilization & Staffing data product. It consumes the analytics Kafka
// topic, projects each event into the analytical Postgres database via the
// idempotent PostgresProjection, and serves only a health endpoint on an admin
// port. It is the single writer of the analytical database and serves no
// reports; the reader (cmd/workforce-reports) is a separate deployable
// (ADR-0010).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inboundkafka "github.com/claudioed/workforce-management/internal/adapters/inbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/analyticsstore"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/bootretry"
	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/telemetry"
	"github.com/claudioed/workforce-management/internal/composition"
)

// Boot errors of the projector's mandatory configuration. Each names its
// variable (ADR-0038): the projector writes only to the analytical database
// and consumes only from real Kafka, with no default behind either.
var (
	errMissingAnalyticsURL = errors.New("ANALYTICS_DATABASE_URL is required: the projector writes only to the analytical database")
	errMissingBrokers      = errors.New("KAFKA_BROKERS is required: the projector consumes the analytics topic")
	errMissingGroup        = errors.New("ANALYTICS_CONSUMER_GROUP is required: a stable Kafka group id for the projector; there is no default")
)

// config is the validated mandatory environment of the projector.
type config struct {
	analyticsURL string
	brokers      []string
	group        string
}

// loadConfig reads and validates the mandatory environment through getenv
// (os.Getenv in main). Every problem is reported at once.
func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		analyticsURL: getenv("ANALYTICS_DATABASE_URL"),
		brokers:      composition.ParseBrokers(getenv("KAFKA_BROKERS")),
		group:        getenv("ANALYTICS_CONSUMER_GROUP"),
	}
	var errs []error
	if c.analyticsURL == "" {
		errs = append(errs, errMissingAnalyticsURL)
	}
	if len(c.brokers) == 0 {
		errs = append(errs, errMissingBrokers)
	}
	if c.group == "" {
		errs = append(errs, errMissingGroup)
	}
	if err := errors.Join(errs...); err != nil {
		return config{}, err
	}
	return c, nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("workforce-projector exited with error", "error", err)
		os.Exit(1)
	}
}

// setupTelemetry wires OTel non-blocking: a failed setup degrades to
// dropped telemetry, never a projector that won't start, and an absent one
// logs why traces and metrics will not be exported. The returned func
// flushes on shutdown (a no-op when setup degraded to nil).
func setupTelemetry(ctx context.Context, logger *slog.Logger) func() {
	serviceName := getenv("OTEL_SERVICE_NAME", "workforce-projector")
	otlpEndpoint := getenv("OTEL_EXPORTER_OTLP_ENDPOINT", telemetry.DefaultOTLPEndpoint)
	otelShutdown, err := telemetry.Setup(ctx, serviceName, serviceVersion(), otlpEndpoint)
	if err != nil {
		logger.Error("opentelemetry setup degraded", "error", err)
	}
	if otelShutdown != nil {
		return func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := otelShutdown(ctx); err != nil {
				logger.Error("opentelemetry shutdown failed", "error", err)
			}
		}
	}
	logger.Warn("opentelemetry disabled; traces and metrics will not be exported")
	return func() {}
}

// openAnalyticsPool runs the analytical schema's migrations on start (the
// projector owns the analytical schema) and opens the analytics pool, both
// retried: in this fleet EVERY injected pod's FIRST outbound TCP dial
// (here, Postgres) is reset ~10s after the app starts (Istio native
// sidecars). A single attempt turns that known, transient condition into
// CrashLoopBackOff. The retry does not weaken the fail-closed rule: once
// the budget is exhausted this still refuses to boot.
func openAnalyticsPool(ctx context.Context, logger *slog.Logger, analyticsURL, migrationsPath string) (*pgxpool.Pool, error) {
	if err := bootretry.Retry(ctx, logger, "run migrations", func() error {
		return postgres.Migrate(analyticsURL, migrationsPath)
	}); err != nil {
		return nil, err
	}
	var pool *pgxpool.Pool
	if err := bootretry.Retry(ctx, logger, "open analytics postgres pool", func() error {
		var err error
		pool, err = analyticsstore.NewPool(ctx, analyticsURL)
		return err
	}); err != nil {
		return nil, err
	}
	return pool, nil
}

// newAnalyticsPipeline wires the projector's single writing pipeline: the
// idempotent Postgres projection, the consumed-events dedupe repo, and the
// Kafka consumer reading the analytics topic under the analytics consumer
// group.
func newAnalyticsPipeline(pool *pgxpool.Pool, brokers []string, group string, logger *slog.Logger) *inboundkafka.AnalyticsConsumer {
	projection := analyticsstore.NewPostgresProjection(pool)
	consumed := analyticsstore.NewConsumedEventsRepo(pool)
	return inboundkafka.NewAnalyticsConsumer(brokers, outboundkafka.AnalyticsTopic, group, projection, consumed, logger)
}

// newAdminServer builds the projector's admin server: only a /healthz on
// the admin port, so Kubernetes probes can reach the process. It reports
// the process, not the consumer: a consumer loop that stops ends the
// process (superviseConsumer, gap fix D13), so a live /healthz always sits
// over a running consumer.
func newAdminServer(adminAddr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return &http.Server{Addr: adminAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

func run() error {
	logger := newLogger(getenv("LOG_LEVEL", "info"))
	slog.SetDefault(logger)

	rootCtx := context.Background()
	// Fail fast, before telemetry, any connection and the admin listener.
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	defer setupTelemetry(rootCtx, logger)()

	adminAddr := getenv("ADMIN_ADDR", ":8091")
	analyticsURL := cfg.analyticsURL
	kafkaBrokers := cfg.brokers
	migrationsPath := getenv("ANALYTICS_MIGRATIONS_PATH", "migrations/analytics")

	pool, err := openAnalyticsPool(rootCtx, logger, analyticsURL, migrationsPath)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := analyticsstore.RecordPoolStats(pool); err != nil {
		logger.Error("analytics pgxpool metrics unavailable", "error", err)
	}

	consumer := newAnalyticsPipeline(pool, kafkaBrokers, cfg.group, logger)
	defer func() { _ = consumer.Close() }()

	srv := newAdminServer(adminAddr)

	go func() {
		logger.Info("projector admin server listening", "addr", adminAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("projector admin server failed", "error", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	logger.Info("analytics consumer starting", "topic", outboundkafka.AnalyticsTopic, "group", cfg.group, "brokers", kafkaBrokers)
	consumerErr := superviseConsumer(consumer.Run, stop)
	if consumerErr != nil {
		logger.Error("analytics consumer stopped; exiting so the pod restarts", "error", consumerErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(consumerErr, srv.Shutdown(ctx))
}

// errConsumerStopped marks a consumer loop that ended without a shutdown
// signal. run() returns it, so main exits non-zero.
var errConsumerStopped = errors.New("analytics consumer stopped")

// superviseConsumer runs consume in the background and blocks until a
// shutdown signal arrives or the consumer loop exits on its own.
//
// On a signal it cancels the consumer and returns nil (a requested
// shutdown). When the loop exits first (a commit or DLQ failure, or any
// return without a signal) it returns an error wrapping errConsumerStopped
// and the loop's own error. The projector then exits non-zero instead of
// keeping its admin /healthz green over a dead consumer (gap fix D13): the
// only thing this process does is consume, so a stopped consumer means a
// stopped projector, and Kubernetes restarts it.
func superviseConsumer(consume func(context.Context) error, stop <-chan os.Signal) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- consume(ctx) }()

	select {
	case <-stop:
		return nil
	case err := <-done:
		if err == nil {
			return errConsumerStopped
		}
		return fmt.Errorf("%w: %w", errConsumerStopped, err)
	}
}

// version is the service version reported as the OTel service.version resource
// attribute, overridable at build time (-ldflags "-X main.version=...") and
// otherwise falling back to the SERVICE_VERSION env var, then "dev".
var version = ""

func serviceVersion() string {
	if version != "" {
		return version
	}
	return getenv("SERVICE_VERSION", "dev")
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

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
