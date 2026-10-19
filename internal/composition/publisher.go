// Package composition holds wiring helpers shared by the module's
// composition roots (cmd/workforce and cmd/mcp). It exists so that every
// deployable that executes write use cases builds its ports.EventPublisher
// the SAME way — in particular so a LaborAssigned / LaborReassigned raised
// through the MCP adapter is stored in the transactional outbox exactly like
// one raised through REST (ADR-0008, ADR-0016).
//
// Postgres and Kafka are the only runtime (ADR-0038): there is no log
// publisher, no direct-to-broker publisher and no file catalogue here.
//
// It reads NO environment: the roots resolve KAFKA_BROKERS,
// OUTBOX_RELAY_INTERVAL and OUTBOX_MAX_ATTEMPTS and pass them in via
// PublisherConfig. Only the cmd/** roots may import this package.
package composition

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/ports"
)

// PublisherConfig is the already-resolved publisher configuration.
type PublisherConfig struct {
	// Brokers is the parsed KAFKA_BROKERS list (mandatory).
	Brokers []string
	// RunRelay asks for the outbox relay to be built (cmd/workforce). The
	// relay is deliberately NOT built by cmd/mcp: only one process drains
	// outbox_events, while every writer process inserts into it.
	RunRelay bool
	// RelayInterval is the relay poll interval (OUTBOX_RELAY_INTERVAL).
	RelayInterval time.Duration
	// MaxAttempts caps how many failed sends an outbox row may accumulate
	// before the relay stops claiming it (OUTBOX_MAX_ATTEMPTS, ADR 0036);
	// 0 means unlimited.
	MaxAttempts int
}

// Built is what BuildEventPublisher returns.
type Built struct {
	// Publisher is the ports.EventPublisher the use cases publish through.
	Publisher ports.EventPublisher
	// Relay is the outbox relay to run alongside the process; nil only when
	// the caller asked for a writer-only publisher (RunRelay false).
	Relay *postgres.OutboxRelay
	// Close releases the publisher's adapters; never nil.
	Close func()
}

// Boot errors of the publisher wiring. Each names its variable.
var (
	errNoPool    = fmt.Errorf("event publisher cannot be built (events leave only through the transactional outbox): %w", ErrNoDatabase)
	errNoBrokers = fmt.Errorf("event publisher cannot be built: %w", ErrNoBrokers)
)

// BuildEventPublisher wires the outbound event publisher: always the
// transactional outbox (ADR-0016) over Postgres. Every domain event is fanned
// to BOTH the integration topic and the analytics topic (ADR-0010), always as
// CloudEvents 1.0 structured-mode messages: both Kafka publishers act only as
// Encoders inside the use case's transaction, and the relay (when RunRelay)
// forwards the stored rows to Kafka, so the store and the topics can no longer
// diverge. A nil pool or an empty broker list is an error, never a fallback.
func BuildEventPublisher(cfg PublisherConfig, pool *pgxpool.Pool, shiftPlans ports.ShiftPlanRepo, logger *slog.Logger) (Built, error) {
	if pool == nil {
		return Built{}, errNoPool
	}
	if len(cfg.Brokers) == 0 {
		return Built{}, errNoBrokers
	}

	// Encoders only: no writer is ever opened for them.
	integration := outboundkafka.NewPublisherWithWriter(nil, shiftPlans)
	analytics := outboundkafka.NewAnalyticsPublisherWithWriter(nil, outboundkafka.NewEventID)
	outbox := postgres.NewOutboxPublisher(pool, integration, analytics)

	if !cfg.RunRelay {
		logger.Info("event publisher configured", "publisher", "kafka", "mode", "outbox-writer-only",
			"topic", outboundkafka.Topic, "analytics_topic", outboundkafka.AnalyticsTopic)
		return Built{Publisher: outbox, Close: func() {}}, nil
	}

	// The relay's topic-less sink is the single Kafka connection this
	// process holds for publishing.
	sink := outboundkafka.NewRelaySink(cfg.Brokers)
	relay := postgres.NewOutboxRelay(pool, sink, logger,
		postgres.WithInterval(cfg.RelayInterval),
		postgres.WithMaxAttempts(cfg.MaxAttempts))
	// workforce.outbox.lag_seconds (ADR-0016): only meaningful where the
	// relay drains the outbox.
	lagGaugeReg, err := postgres.RegisterOutboxLagGauge(pool)
	if err != nil {
		logger.Error("failed to register outbox lag gauge", "error", err)
	}
	logger.Info("event publisher configured", "publisher", "kafka", "mode", "outbox",
		"brokers", cfg.Brokers, "topic", outboundkafka.Topic, "analytics_topic", outboundkafka.AnalyticsTopic,
		"outbox_max_attempts", cfg.MaxAttempts)
	return Built{
		Publisher: outbox,
		Relay:     relay,
		Close: func() {
			if err := sink.Close(); err != nil {
				logger.Error("kafka relay sink close failed", "error", err)
			}
			if lagGaugeReg != nil {
				if err := lagGaugeReg.Unregister(); err != nil {
					logger.Warn("outbox lag gauge unregister failed", "error", err)
				}
			}
		},
	}, nil
}
