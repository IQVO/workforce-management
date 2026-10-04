package composition

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/bootretry"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/filecatalog"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/kafkacatalog"
	"github.com/claudioed/workforce-management/internal/application/ports"
)

// Path-catalogue sources accepted in CatalogueConfig.Source (the
// PATH_CATALOGUE_SOURCE values).
const (
	// CatalogueFromKafka reads the catalogue from the process-path
	// management compacted topic (ADR-0013 addendum).
	CatalogueFromKafka = "kafka"
	// CatalogueFromFile reads the catalogue from a boot-time YAML file.
	CatalogueFromFile = "file"
)

// CatalogueConfig is the already-resolved process-path catalogue
// configuration. The roots read PATH_CATALOGUE_SOURCE,
// PATH_CATALOGUE_FILE and KAFKA_BROKERS; this package reads no environment.
type CatalogueConfig struct {
	// Source is the PATH_CATALOGUE_SOURCE value; anything but "kafka"
	// selects the file.
	Source string
	// File is the PATH_CATALOGUE_FILE path (file source).
	File string
	// Brokers is the parsed KAFKA_BROKERS list (kafka source).
	Brokers []string
}

// BuildCatalogue resolves the process-path catalogue from its selectable
// source. Both deployables that validate caller-supplied path ids
// (cmd/workforce for REST, cmd/mcp for MCP tools) call it, so ADR-0013
// validation is identical on both surfaces and, in both, fails closed at
// boot: a missing or malformed file (or a kafka catalogue that never
// replays) stops the process from starting rather than falling back to a
// partial or empty catalogue — a nil catalogue is therefore only ever a
// unit-test seam, never a runtime state.
//
// consumerCtx must already be live for the kafka source: its consumer's Run
// goroutine is started BEFORE WaitReady is called, otherwise nothing would
// consume while this process waits. The returned *kafkacatalog.Consumer is
// nil for the file source; the caller cancels consumerCtx and closes it on
// shutdown.
func BuildCatalogue(ctx, consumerCtx context.Context, cfg CatalogueConfig, logger *slog.Logger) (ports.PathCatalogue, *kafkacatalog.Consumer, error) {
	if cfg.Source != CatalogueFromKafka {
		// Loaded and validated once at boot, before anything else stands
		// up (mirrors fulfillment-execution's and wes-work-planning's
		// identical contract; see ADR-0013).
		fileCatalogue, err := filecatalog.Load(cfg.File)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load the process-path catalogue: %w", err)
		}
		logger.Info("process-path catalogue loaded", "paths", fileCatalogue.Ids())
		return fileCatalogue, nil, nil
	}

	if len(cfg.Brokers) == 0 || cfg.Brokers[0] == "" {
		return nil, nil, fmt.Errorf("PATH_CATALOGUE_SOURCE=kafka requires KAFKA_BROKERS to be set")
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
