package composition

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/bootretry"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/kafkacatalog"
	"github.com/claudioed/workforce-management/internal/application/ports"
)

// CatalogueConfig is the already-resolved process-path catalogue
// configuration. The roots read KAFKA_BROKERS; this package reads no
// environment. The catalogue always comes from process-path-management's
// topic (ADR-0030, ADR-0038): there is no file source any more.
type CatalogueConfig struct {
	// Brokers is the parsed KAFKA_BROKERS list (mandatory).
	Brokers []string
}

// errNoCatalogueBrokers is the boot error of an empty broker list.
var errNoCatalogueBrokers = fmt.Errorf("process-path catalogue cannot be built: %w", ErrNoBrokers)

// BuildCatalogue resolves the process-path catalogue from its Kafka topic.
// Both deployables that validate caller-supplied path ids (cmd/workforce for
// REST, cmd/mcp for MCP tools) call it, so ADR-0013 validation is identical
// on both surfaces and, in both, fails closed at boot: a kafka catalogue that
// never replays stops the process from starting rather than falling back to
// a partial or empty catalogue — a nil catalogue is therefore only ever a
// unit-test seam, never a runtime state.
//
// consumerCtx must already be live: the consumer's Run goroutine is started
// BEFORE WaitReady is called, otherwise nothing would consume while this
// process waits. The returned *kafkacatalog.Consumer is the one the caller
// cancels consumerCtx for and closes on shutdown.
func BuildCatalogue(ctx, consumerCtx context.Context, cfg CatalogueConfig, logger *slog.Logger) (ports.PathCatalogue, *kafkacatalog.Consumer, error) {
	if len(cfg.Brokers) == 0 || cfg.Brokers[0] == "" {
		return nil, nil, errNoCatalogueBrokers
	}
	// Retried: this consumer's construction dials Kafka directly to
	// determine its readiness watermark (newTargetOffsets), and in this
	// fleet EVERY injected pod's first outbound dial is reset ~10s after
	// start (Istio native sidecars). One attempt here turns that
	// transient into the same CrashLoopBackOff the Postgres boot dial is
	// guarded against.
	var kafkaCatalogue *kafkacatalog.Consumer
	if err := bootretry.Retry(ctx, logger, "connect process-path catalogue kafka consumer", func() error {
		var newErr error
		kafkaCatalogue, newErr = kafkacatalog.NewConsumer(ctx, cfg.Brokers, logger)
		return newErr
	}); err != nil {
		return nil, nil, fmt.Errorf("failed to start the Kafka-sourced process-path catalogue: %w", err)
	}
	logger.Info("process-path catalogue source configured", "source", "kafka", "topic", kafkacatalog.Topic)
	go func() {
		logger.Info("process-path catalogue consumer running", "topic", kafkacatalog.Topic)
		if err := kafkaCatalogue.Run(consumerCtx); err != nil {
			logger.Error("process-path catalogue consumer stopped", "error", err)
		}
	}()

	logger.Info("waiting for the process-path catalogue to replay its initial history before accepting traffic")
	waitCtx, waitCancel := context.WithTimeout(context.Background(), kafkacatalog.WaitReadyTimeout)
	err := kafkaCatalogue.WaitReady(waitCtx)
	waitCancel()
	if err != nil {
		return nil, nil, fmt.Errorf("process-path catalogue did not become ready within %s: %w", kafkacatalog.WaitReadyTimeout, err)
	}
	logger.Info("process-path catalogue is ready", "paths", kafkaCatalogue.Ids())
	return kafkaCatalogue, kafkaCatalogue, nil
}
