package main

import "testing"

// TestOutboxMaxAttemptsFromEnv pins OUTBOX_MAX_ATTEMPTS (gap fix D12,
// ADR 0036): unset means the fleet default of 20, 0 means unlimited, and an
// invalid or negative value falls back to the default instead of disabling
// the cap.
func TestOutboxMaxAttemptsFromEnv(t *testing.T) {
	if defaultOutboxMaxAttempts != 20 {
		t.Fatalf("defaultOutboxMaxAttempts = %d, want the fleet default 20", defaultOutboxMaxAttempts)
	}
	tests := []struct {
		name string
		env  string
		want int
	}{
		{"unset defaults to 20", "", 20},
		{"explicit cap", "5", 5},
		{"one is a valid cap", "1", 1},
		{"surrounding spaces are tolerated", " 7 ", 7},
		{"zero means unlimited", "0", 0},
		{"negative falls back", "-1", 20},
		{"garbage falls back", "many", 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(outboxMaxAttemptsEnv, tt.env)
			if got := envNonNegativeIntOrDefault(outboxMaxAttemptsEnv, defaultOutboxMaxAttempts); got != tt.want {
				t.Fatalf("envNonNegativeIntOrDefault(%q) = %d, want %d", tt.env, got, tt.want)
			}
		})
	}
}

// TestPublisherConfigFromEnv_CarriesTheOutboxCap proves the cap reaches the
// shared composition (which passes it to the relay as WithMaxAttempts).
func TestPublisherConfigFromEnv_CarriesTheOutboxCap(t *testing.T) {
	t.Setenv(outboxMaxAttemptsEnv, "")
	brokers := []string{"b:9092"}
	if got := publisherConfigFromEnv(brokers).MaxAttempts; got != 20 {
		t.Fatalf("default MaxAttempts = %d, want 20", got)
	}
	t.Setenv(outboxMaxAttemptsEnv, "3")
	cfg := publisherConfigFromEnv(brokers)
	if cfg.MaxAttempts != 3 {
		t.Fatalf("MaxAttempts = %d, want 3 from OUTBOX_MAX_ATTEMPTS", cfg.MaxAttempts)
	}
	if !cfg.RunRelay || len(cfg.Brokers) != 1 {
		t.Fatalf("cmd/workforce always runs the relay on the given brokers, got %+v", cfg)
	}
}
