// Package composition holds wiring helpers shared by the module's
// composition roots (cmd/workforce and cmd/mcp). It exists so that every
// deployable that executes write use cases builds its ports.EventPublisher
// the SAME way — in particular so a LaborAssigned / LaborReassigned raised
// through the MCP adapter is stored in the transactional outbox exactly like
// one raised through REST (ADR-0008, ADR-0016).
//
// It reads NO environment: the roots resolve EVENT_PUBLISHER, KAFKA_BROKERS
// and OUTBOX_RELAY_INTERVAL and pass them in via PublisherConfig. Only the
// cmd/** roots may import this package.
package composition

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/ports"
)

// Publisher kinds accepted in PublisherConfig.Kind (the EVENT_PUBLISHER
// values).
const (
	// KafkaPublisher selects Kafka (transactional outbox when Postgres is
	// configured).
	KafkaPublisher = "kafka"
	// LogPublisher selects the log-only publisher.
	LogPublisher = "log"
)

// PublisherConfig is the already-resolved publisher configuration.
type PublisherConfig struct {
	// Kind is the EVENT_PUBLISHER value ("kafka" or "log"; empty means log).
	Kind string
	// Brokers is the parsed KAFKA_BROKERS list.
	Brokers []string
	// RunRelay asks for the outbox relay to be built (cmd/workforce). The
	// relay is deliberately NOT built by cmd/mcp: only one process drains
	// outbox_events, while every writer process inserts into it.
	RunRelay bool
	// RelayInterval is the relay poll interval (OUTBOX_RELAY_INTERVAL).
	RelayInterval time.Duration
}

// Built is what BuildEventPublisher returns.
type Built struct {
	// Publisher is the ports.EventPublisher the use cases publish through.
	Publisher ports.EventPublisher
	// Relay is the outbox relay to run alongside the process; nil when none
	// was requested or applicable.
	Relay *postgres.OutboxRelay
	// Close releases the publisher's adapters; never nil.
	Close func()
}

// BuildEventPublisher wires the outbound event publisher.
//
// An empty or "log" Kind selects the log publisher, so a local dev run with
// no Kafka is still fully functional; any other value than "kafka"/"log" is
// an error. With Kind "kafka" every domain event is fanned to BOTH the
// integration topic and the analytics topic (ADR-0010), always as
// CloudEvents 1.0 structured-mode messages (there is no envelope toggle):
//
//   - with Postgres configured (pool != nil) the use cases publish into the
//     transactional outbox (ADR-0016): both Kafka publishers act only as
//     Encoders inside the use case's transaction, and the relay (when
//     RunRelay) forwards the stored rows to Kafka. The store and the
//     topics can no longer diverge.
//   - with in-memory adapters (pool == nil) events go straight to the
//     broker through MultiPublisher — there is no transaction to bind
//     them to.
func BuildEventPublisher(cfg PublisherConfig, pool *pgxpool.Pool, shiftPlans ports.ShiftPlanRepo, logger *slog.Logger) (Built, error) {
	switch cfg.Kind {
	case "", LogPublisher:
		logger.Info("event publisher configured", "publisher", "log")
		return Built{Publisher: events.NewLogPublisher(logger), Close: func() {}}, nil
	case KafkaPublisher:
	default:
		return Built{}, fmt.Errorf("unknown EVENT_PUBLISHER %q (want kafka or log)", cfg.Kind)
	}

	if pool == nil {
		integration := outboundkafka.NewPublisher(cfg.Brokers, shiftPlans)
		analytics := outboundkafka.NewAnalyticsPublisher(cfg.Brokers, outboundkafka.NewEventID)
		logger.Info("event publisher configured", "publisher", "kafka", "mode", "direct",
			"brokers", cfg.Brokers, "topic", outboundkafka.Topic, "analytics_topic", outboundkafka.AnalyticsTopic)
		return Built{
			Publisher: events.NewMultiPublisher(integration, analytics),
			Close: func() {
				if err := integration.Close(); err != nil {
					logger.Error("kafka publisher close failed", "error", err)
				}
				if err := analytics.Close(); err != nil {
					logger.Error("kafka analytics publisher close failed", "error", err)
				}
			},
		}, nil
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
	relay := postgres.NewOutboxRelay(pool, sink, logger, postgres.WithInterval(cfg.RelayInterval))
	// workforce.outbox.lag_seconds (ADR-0016): only meaningful where the
	// relay drains the outbox.
	lagGaugeReg, err := postgres.RegisterOutboxLagGauge(pool)
	if err != nil {
		logger.Error("failed to register outbox lag gauge", "error", err)
	}
	logger.Info("event publisher configured", "publisher", "kafka", "mode", "outbox",
		"brokers", cfg.Brokers, "topic", outboundkafka.Topic, "analytics_topic", outboundkafka.AnalyticsTopic)
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
