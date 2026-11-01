package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/claudioed/workforce-management/internal/composition"
)

// envSettings groups run()'s plain environment reads so the function itself
// stays inside the linter's length budget.
type envSettings struct {
	databaseURL           string
	migrationsDatabaseURL string
	httpAddr              string
	migrationsPath        string
	maxHoursPerShift      float64
}

// envSettingsFromEnv reads the optional configuration; see openPostgresPool
// and ADR-0025 for MIGRATIONS_DATABASE_URL.
func envSettingsFromEnv(databaseURL string) envSettings {
	return envSettings{
		databaseURL:           databaseURL,
		migrationsDatabaseURL: envOrDefault("MIGRATIONS_DATABASE_URL", databaseURL),
		httpAddr:              envOrDefault("HTTP_ADDR", ":8080"),
		migrationsPath:        envOrDefault("MIGRATIONS_PATH", "migrations"),
		maxHoursPerShift:      envFloatOrDefault("MAX_HOURS_PER_SHIFT", 8.0),
	}
}

// OUTBOX_MAX_ATTEMPTS caps how many failed sends an outbox row may
// accumulate before the relay stops claiming it (gap fix D12, ADR 0036).
// 0 = unlimited.
const (
	outboxMaxAttemptsEnv     = "OUTBOX_MAX_ATTEMPTS"
	defaultOutboxMaxAttempts = 20
)

// publisherConfigFromEnv resolves OUTBOX_RELAY_INTERVAL and
// OUTBOX_MAX_ATTEMPTS (KAFKA_BROKERS arrives validated) for the shared
// composition.BuildEventPublisher. This process owns the outbox relay
// (RunRelay): cmd/mcp only inserts.
func publisherConfigFromEnv(brokers []string) composition.PublisherConfig {
	return composition.PublisherConfig{
		Brokers:       brokers,
		RunRelay:      true,
		RelayInterval: envDurationOrDefault("OUTBOX_RELAY_INTERVAL", time.Second),
		MaxAttempts:   envNonNegativeIntOrDefault(outboxMaxAttemptsEnv, defaultOutboxMaxAttempts),
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloatOrDefault(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		slog.Error("invalid float env var", "key", key, "error", err)
		os.Exit(1)
	}
	return f
}

// envNonNegativeIntOrDefault parses key as a non-negative integer, falling
// back to def when it is unset and, with a warning, when it is malformed or
// negative: like the relay interval it is a tuning knob, so it never fails
// the boot. 0 is a valid value (OUTBOX_MAX_ATTEMPTS uses it for
// "unlimited").
func envNonNegativeIntOrDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		slog.Warn("invalid integer env var, using default", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

// envDurationOrDefault parses key as a time.Duration, falling back on
// absence or a malformed/non-positive value: the relay interval is a tuning
// knob, not a contract, so it never fails the boot.
func envDurationOrDefault(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("invalid duration env var, using default", "key", key, "value", v, "default", def)
		return def
	}
	return d
}
