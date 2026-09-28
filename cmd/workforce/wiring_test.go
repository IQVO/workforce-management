package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestMigrationsDatabaseURLFallback proves the fallback wiring run()
// applies before calling openPostgresPool:
// envOrDefault("MIGRATIONS_DATABASE_URL", databaseURL) must return
// MIGRATIONS_DATABASE_URL's own value when it is set, and databaseURL
// itself (DATABASE_URL) when it is unset. This is the exact env-lookup
// line the fix for the PgBouncer/pg_advisory_lock incompatibility (ADR
// 0025-migrations-direct-postgres-connection.md, ported from
// order-management's ADR-0029) depends on: any environment that doesn't
// provision the split (local dev, CI integration tests, a cluster whose
// Terraform predates this fix) must keep working exactly as before,
// using DATABASE_URL for everything including migrations.
func TestMigrationsDatabaseURLFallback(t *testing.T) {
	const databaseURL = "postgres://u:***@pgbouncer.example:6432/workforce_management?sslmode=disable"

	t.Run("falls back to DATABASE_URL when MIGRATIONS_DATABASE_URL is unset", func(t *testing.T) {
		t.Setenv("MIGRATIONS_DATABASE_URL", "")
		os.Unsetenv("MIGRATIONS_DATABASE_URL")

		got := envOrDefault("MIGRATIONS_DATABASE_URL", databaseURL)
		if got != databaseURL {
			t.Fatalf("envOrDefault fallback = %q, want the DATABASE_URL value %q", got, databaseURL)
		}
	})

	t.Run("uses MIGRATIONS_DATABASE_URL when set, not DATABASE_URL", func(t *testing.T) {
		const direct = "postgres://u:***@postgres-postgresql.example:5432/workforce_management?sslmode=disable"
		t.Setenv("MIGRATIONS_DATABASE_URL", direct)

		got := envOrDefault("MIGRATIONS_DATABASE_URL", databaseURL)
		if got != direct {
			t.Fatalf("envOrDefault = %q, want the direct MIGRATIONS_DATABASE_URL value %q (must NOT silently keep using DATABASE_URL/PgBouncer)", got, direct)
		}
		if got == databaseURL {
			t.Fatal("MIGRATIONS_DATABASE_URL and DATABASE_URL collapsed to the same value — the whole point of this env var is that it differs")
		}
	})
}

// TestOpenPostgresPool_UsesMigrationsDatabaseURLNotDatabaseURLForMigrations
// proves openPostgresPool itself — not just the env-var read above —
// actually threads migrationsDatabaseURL into the migration step and
// databaseURL into the pgxpool, rather than the two ever being
// conflated. Gives databaseURL an address nothing listens on (so
// opening the pgxpool, which happens AFTER migrations succeed, would
// hang/fail loudly if ever reached) and migrationsDatabaseURL a
// schemeless string that migrate.New rejects immediately with a
// distinctive parse error ("failed to parse scheme from database URL")
// — if openPostgresPool ignored migrationsDatabaseURL and ran
// migrations against databaseURL instead, this test would see a
// dial/"connection refused" error, not the parse error.
func TestOpenPostgresPool_UsesMigrationsDatabaseURLNotDatabaseURLForMigrations(t *testing.T) {
	const (
		bogusMigrationsURL = "not-a-valid-connection-string"
		unreachableAppURL  = "postgres://u:***@127.0.0.1:1/workforce_management?sslmode=disable&connect_timeout=1"
	)

	_, err := openPostgresPool(context.Background(), quietLogger(), unreachableAppURL, bogusMigrationsURL, migrationsDirForTest(t))
	if err == nil {
		t.Fatal("a malformed MIGRATIONS_DATABASE_URL must fail boot")
	}
	if !strings.Contains(err.Error(), "parse scheme") {
		t.Fatalf("err = %v — expected the bogus-URL parse error from migrate.New; a \"connection refused\"/dial error here would mean migrations ran against databaseURL/unreachableAppURL instead of migrationsDatabaseURL", err)
	}
}

// migrationsDirForTest returns the repo's real migrations directory so
// migrate.New has a valid source to pair with the (deliberately bogus)
// connection string above — the test exercises the connection-string
// parse failure, not a missing-migrations-directory failure.
func migrationsDirForTest(t *testing.T) string {
	t.Helper()
	return "../../migrations"
}
