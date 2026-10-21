package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/claudioed/workforce-management/internal/composition"
)

// LABOR_PERFORMANCE_MODE values. kafka-cache (the default) feeds the measured
// rate from an event-fed local cache of labor-performance's
// TaskPerformanceRecorded (ADR 0019); http is the REST rollback of that edge
// (docs/rollback-switches.md R10), a REST sync edge that ADR-0038 leaves
// unchanged. The permissive no-op is gone.
const (
	laborModeKafkaCache = "kafka-cache"
	laborModeHTTP       = "http"
)

// bootConfig is the mandatory configuration of cmd/workforce: real Postgres
// and real Kafka, with no default and no degraded mode behind any of it.
type bootConfig struct {
	databaseURL string
	brokers     []string
	// laborMode is the validated LABOR_PERFORMANCE_MODE; laborBaseURL is only
	// meaningful in http mode.
	laborMode    string
	laborBaseURL string
}

// checkLaborPerformanceMode validates LABOR_PERFORMANCE_MODE (already
// defaulted to kafka-cache by the caller).
func checkLaborPerformanceMode(mode, baseURL string) error {
	if mode == laborModeKafkaCache {
		return nil
	}
	if mode == laborModeHTTP {
		if baseURL == "" {
			return errors.New("LABOR_PERFORMANCE_MODE=http requires LABOR_PERFORMANCE_BASE_URL")
		}
		return nil
	}
	if mode == "permissive" {
		return errors.New("LABOR_PERFORMANCE_MODE=permissive is not supported: this context runs on real Kafka (use kafka-cache, the default)")
	}
	return fmt.Errorf("unknown LABOR_PERFORMANCE_MODE %q (want kafka-cache, or http as the REST rollback)", mode)
}

// loadBootConfig reads and validates the mandatory environment. Every problem
// is reported at once, each naming its variable, so the process fails fast at
// boot, before it opens a connection or binds the listener.
func loadBootConfig() (bootConfig, error) {
	cfg := bootConfig{
		databaseURL:  os.Getenv("DATABASE_URL"),
		brokers:      composition.ParseBrokers(os.Getenv("KAFKA_BROKERS")),
		laborMode:    envOrDefault("LABOR_PERFORMANCE_MODE", laborModeKafkaCache),
		laborBaseURL: os.Getenv("LABOR_PERFORMANCE_BASE_URL"),
	}
	var errs []error
	if cfg.databaseURL == "" {
		errs = append(errs, composition.ErrNoDatabase)
	}
	if len(cfg.brokers) == 0 {
		errs = append(errs, composition.ErrNoBrokers)
	}
	errs = append(errs,
		composition.CheckEventPublisher(os.Getenv("EVENT_PUBLISHER")),
		composition.CheckCatalogueSource(os.Getenv("PATH_CATALOGUE_SOURCE")),
		checkLaborPerformanceMode(cfg.laborMode, cfg.laborBaseURL),
	)
	if err := errors.Join(errs...); err != nil {
		return bootConfig{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}
