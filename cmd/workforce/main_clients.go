package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/bootretry"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/fulfillmentexecution"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/laborperformance"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/laborperformancecache"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/telemetry"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/resilience"
)

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
