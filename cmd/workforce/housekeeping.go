package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
)

// housekeepingSettings is the sweeper's configuration (ADR-0028). A zero
// duration disables the corresponding behaviour: interval 0 disables the
// whole sweeper, TTL/retention 0 keep that table's rows forever.
type housekeepingSettings struct {
	interval        time.Duration
	idempotencyTTL  time.Duration
	outboxRetention time.Duration
}

// housekeepingSettingsFromEnv reads HOUSEKEEPING_INTERVAL (default 1h),
// IDEMPOTENCY_KEY_TTL (default 24h) and OUTBOX_RETENTION (default 168h = 7d).
func housekeepingSettingsFromEnv(logger *slog.Logger) housekeepingSettings {
	return housekeepingSettings{
		interval:        envDurationAllowZero(logger, "HOUSEKEEPING_INTERVAL", postgres.DefaultSweepInterval),
		idempotencyTTL:  envDurationAllowZero(logger, "IDEMPOTENCY_KEY_TTL", postgres.DefaultIdempotencyKeyTTL),
		outboxRetention: envDurationAllowZero(logger, "OUTBOX_RETENTION", postgres.DefaultOutboxRetention),
	}
}

// envDurationAllowZero parses a Go duration env var. Unset yields def; an
// invalid or negative value logs a warning and yields def. "0" is valid and
// means "disabled" (or "no wait") to the caller.
func envDurationAllowZero(logger *slog.Logger, key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		logger.Warn("invalid duration env var, using default", "key", key, "value", raw, "default", def.String(), "error", err)
		return def
	}
	return d
}

// startSweeper runs the housekeeping Sweeper (ADR-0028) in a goroutine and
// returns a stop func that cancels it and waits, bounded by
// shutdownTimeout, for the current pass to finish. With interval 0 it starts
// nothing and returns a no-op. The stop func is idempotent.
func startSweeper(pool *pgxpool.Pool, cfg housekeepingSettings, logger *slog.Logger) func() {
	if cfg.interval <= 0 {
		logger.Info("housekeeping sweeper disabled (HOUSEKEEPING_INTERVAL=0)")
		return func() {}
	}
	sweeper := postgres.NewSweeper(pool,
		postgres.WithSweeperLogger(logger),
		postgres.WithSweepInterval(cfg.interval),
		postgres.WithIdempotencyKeyTTL(cfg.idempotencyTTL),
		postgres.WithOutboxRetention(cfg.outboxRetention),
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sweeper.Run(ctx) // only ever returns nil, on cancellation
	}()
	logger.Info("housekeeping sweeper started", "interval", cfg.interval,
		"idempotency_key_ttl", cfg.idempotencyTTL, "outbox_retention", cfg.outboxRetention)
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(shutdownTimeout):
			logger.Warn("housekeeping sweeper did not stop before the shutdown deadline")
		}
	}
}
