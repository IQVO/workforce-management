package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	inbound "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
)

// Graceful-shutdown timing (ADR-0022 §8).
const (
	// defaultShutdownDrainDelay is how long the process keeps serving after
	// readiness flips to not-ready: two readinessProbe periods
	// (charts values.yaml readinessProbe.periodSeconds = 5), so the kubelet
	// has certainly observed /readyz = 503 and endpoint removal has begun
	// before the HTTP listener closes. Override with SHUTDOWN_DRAIN_DELAY
	// ("0" disables the wait, which is what tests want).
	defaultShutdownDrainDelay = 10 * time.Second

	// shutdownTimeout bounds everything after the drain delay: the HTTP
	// drain AND the sweeper / outbox-relay stop. With the drain delay and
	// the 5s telemetry flush it stays inside the chart's
	// terminationGracePeriodSeconds (30s).
	shutdownTimeout = 10 * time.Second
)

// shutdownDrainDelayFromEnv reads SHUTDOWN_DRAIN_DELAY; unlike the tuning
// knobs elsewhere "0" is valid and honoured (no drain wait).
func shutdownDrainDelayFromEnv(logger *slog.Logger) time.Duration {
	return envDurationAllowZero(logger, "SHUTDOWN_DRAIN_DELAY", defaultShutdownDrainDelay)
}

// shutdownSequence is the ADR-0022 §8 graceful-shutdown order, expressed as
// data so the order itself is unit-testable:
//
//  1. flip readiness to not-ready (/readyz -> 503);
//  2. WAIT drainDelay — the window in which a Kubernetes readinessProbe
//     (period 5s) observes the flip and stops routing NEW traffic. Without
//     this wait the listener closed in the same instant and the flip was
//     never observable;
//  3. http.Server.Shutdown — stop accepting, drain in-flight requests;
//  4. stop the Kafka consumers (catalogue, measured-rate cache);
//  5. stop the housekeeping sweeper;
//  6. stop the outbox relay and wait for its in-flight pass;
//  7. (the caller's defers then close the publisher, and the Postgres pool
//     LAST).
type shutdownSequence struct {
	logger        *slog.Logger
	readiness     *inbound.Readiness
	drainDelay    time.Duration
	timeout       time.Duration
	httpShutdown  func(ctx context.Context) error
	stopConsumers func()
	stopSweeper   func()
	stopRelay     func(ctx context.Context)
}

// run executes the sequence and returns the HTTP shutdown error.
func (s shutdownSequence) run() error {
	s.readiness.SetNotReady()
	if s.drainDelay > 0 {
		s.logger.Info("readiness flipped to not-ready; draining before closing the listener", "drain_delay", s.drainDelay)
		time.Sleep(s.drainDelay)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	err := s.httpShutdown(ctx)
	s.stopConsumers()
	s.stopSweeper()
	s.stopRelay(ctx)
	return err
}

// serveWorkforce runs the HTTP server until a signal arrives or the
// listener fails, with the outbox relay draining alongside, then drains
// everything under the ADR-0022 §8 shutdown sequence.
func serveWorkforce(logger *slog.Logger, httpAddr string, server *http.Server, readiness *inbound.Readiness, relay *postgres.OutboxRelay,
	stopConsumers func(), stopSweeper func()) error {
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", httpAddr)
		serverErr <- server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// The outbox relay (ADR 0016) runs alongside the HTTP server in the
	// same process, draining outbox_events onto both Kafka topics. It is
	// only wired in kafka mode (see composition.BuildEventPublisher).
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
		return shutdownSequence{
			logger:        logger,
			readiness:     readiness,
			drainDelay:    shutdownDrainDelayFromEnv(logger),
			timeout:       shutdownTimeout,
			httpShutdown:  server.Shutdown,
			stopConsumers: stopConsumers,
			stopSweeper:   stopSweeper,
			// Let the relay finish its in-flight pass so an event
			// committed by a request that completed just before
			// shutdown is not stranded until the next pod boots.
			stopRelay: func(ctx context.Context) {
				stopRelay()
				select {
				case <-relayDone:
				case <-ctx.Done():
					logger.Warn("outbox relay did not stop before the shutdown deadline")
				}
			},
		}.run()
	}
	return nil
}
