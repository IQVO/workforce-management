package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	inboundkafka "github.com/claudioed/workforce-management/internal/adapters/inbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
)

// startLabourPlanConsumer builds and starts the labour-plan consumer of ADR
// 0043: it keeps labour_requirements, the local copy of warehouse-planning's
// LabourPlanCommitted, on the stable group LABOUR_PLAN_CONSUMER_GROUP. It is
// always on (Kafka is part of the runtime, ADR-0038) and only this API binary
// starts it: cmd/mcp and cmd/a2a read the copy and never consume. The claim on
// the CloudEvents id and the upsert share one unit of work. The returned
// cancel stops its Run goroutine; the step-4 shutdown action
// (consumerStopper) cancels it and closes the consumer.
func startLabourPlanConsumer(cfg bootConfig, pool *pgxpool.Pool, requirements ports.LabourRequirements, uow ports.UnitOfWork, logger *slog.Logger) (*inboundkafka.LabourPlanConsumer, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	record := &usecases.RecordLabourRequirement{
		Requirements: requirements,
		Processed:    postgres.NewLabourPlanConsumedEvents(pool),
		UnitOfWork:   uow,
	}
	consumer := inboundkafka.NewLabourPlanConsumer(cfg.brokers, inboundkafka.LabourPlanTopic, cfg.labourPlanGroup, record, logger)
	go func() {
		logger.Info("labour-plan consumer running", "topic", inboundkafka.LabourPlanTopic, "group", cfg.labourPlanGroup)
		if err := consumer.Run(ctx); err != nil {
			logger.Error("labour-plan consumer stopped: the labour-plan copy is no longer updated", "error", err)
		}
	}()
	return consumer, cancel
}
