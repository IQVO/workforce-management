package main

import (
	"context"
	"log/slog"
	"testing"
)

// TestNewLogger_AcceptsWarningAlias pins gap fix D22: LOG_LEVEL=warning
// means warn in every binary of the fleet (cmd/workforce used to fall back
// to info for it, unlike its three siblings).
func TestNewLogger_AcceptsWarningAlias(t *testing.T) {
	for _, level := range []string{"warn", "warning", "WARNING"} {
		logger := newLogger(level)
		if !logger.Enabled(context.Background(), slog.LevelWarn) || logger.Enabled(context.Background(), slog.LevelInfo) {
			t.Fatalf("LOG_LEVEL=%q must select the warn level", level)
		}
	}
	if !newLogger("bogus").Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("an unknown LOG_LEVEL must still default to info")
	}
}
