// Command workforce-reports is the READER composition root of the workforce
// Labor Utilization & Staffing data product. It opens the analytical Postgres
// database over a read-only pool and serves the labor report and its freshness
// over REST. It writes nothing: the writer (cmd/workforce-projector) is a
// separate deployable and owns the schema (ADR-0010).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inboundhttp "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/analyticsstore"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/bootretry"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/telemetry"
)

// errMissingAnalyticsURL is returned when ANALYTICS_DATABASE_URL is unset.
var errMissingAnalyticsURL = errors.New("ANALYTICS_DATABASE_URL is required")

func main() {
	if err := run(); err != nil {
		slog.Error("workforce-reports exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logger := newLogger(getenv("LOG_LEVEL", "info"))
	slog.SetDefault(logger)

	rootCtx := context.Background()
	serviceName := getenv("OTEL_SERVICE_NAME", inboundhttp.DefaultReportsServiceName)
	otlpEndpoint := getenv("OTEL_EXPORTER_OTLP_ENDPOINT", telemetry.DefaultOTLPEndpoint)
	otelShutdown, err := telemetry.Setup(rootCtx, serviceName, serviceVersion(), otlpEndpoint)
	if err != nil {
		logger.Error("opentelemetry setup degraded", "error", err)
	}
	if otelShutdown != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := otelShutdown(ctx); err != nil {
				logger.Error("opentelemetry shutdown failed", "error", err)
			}
		}()
	} else {
		logger.Warn("opentelemetry disabled; traces and metrics will not be exported")
	}

	httpAddr := getenv("HTTP_ADDR", ":8092")
	analyticsURL := os.Getenv("ANALYTICS_DATABASE_URL")
	if analyticsURL == "" {
		return errMissingAnalyticsURL
	}

	// Read-only pool: even a bug in the reader cannot mutate the read model, on
	// top of the read-only database role ANALYTICS_DATABASE_URL should use.
	//
	// Retried: in this fleet EVERY injected pod's FIRST outbound TCP dial
	// (here, Postgres) is reset ~10s after the app starts (Istio native
	// sidecars). A single attempt turns that known, transient condition
	// into CrashLoopBackOff. The retry does not weaken the fail-closed
	// rule: once the budget is exhausted this still refuses to boot.
	var pool *pgxpool.Pool
	if err := bootretry.Retry(rootCtx, logger, "open read-only analytics postgres pool", func() error {
		var err error
		pool, err = analyticsstore.NewReadOnlyPool(rootCtx, analyticsURL)
		return err
	}); err != nil {
		return err
	}
	defer pool.Close()
	if err := analyticsstore.RecordPoolStats(pool); err != nil {
		logger.Error("analytics pgxpool metrics unavailable", "error", err)
	}

	handlers := &inboundhttp.ReportsHandlers{Store: analyticsstore.NewPostgresReport(pool)}
	router := inboundhttp.NewReportsRouter(handlers, logger, serviceName)

	srv := &http.Server{Addr: httpAddr, Handler: router, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		logger.Info("reports server listening", "addr", httpAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("reports server failed", "error", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
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
