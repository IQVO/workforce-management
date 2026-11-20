//go:build integration

package main

// The boot-level proof of the READER composition root (ADR-0010, ADR-0037):
// the real run() opens the analytical Postgres through its documented
// read-only pool, serves GET /healthz, GET /reports/labor and
// GET /reports/labor/freshness on HTTP_ADDR
// (docs/docs/operations/configuration.md, cmd/workforce-reports), answers a
// report from facts seeded with the repo's own analytics writer
// (analyticsstore.NewPostgresProjection), rejects a malformed query with the
// documented RFC 7807 400 (apis/openapi-reports.yaml, ADR-0037), and drains
// cleanly on SIGTERM (main.go: stop -> srv.Shutdown -> nil).

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/analyticsstore"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
)

// startEmptyAnalyticsDB boots an EMPTY Postgres, applies the repo's own
// analytical migrations (the reader never migrates: the writer owns the
// schema, ADR-0010) and returns the DSN.
func startEmptyAnalyticsDB(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("analytics_reports_itest"),
		tcpostgres.WithUsername("workforce"),
		tcpostgres.WithPassword("workforce"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres dsn: %v", err)
	}
	_, thisFile, _, _ := runtime.Caller(0)
	migrations := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", "analytics")
	if err := postgres.Migrate(url, migrations); err != nil {
		t.Fatalf("migrate analytics: %v", err)
	}
	return url
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// get performs a GET and decodes a JSON body, returning status and raw body.
func get(t *testing.T, url string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(raw)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// stopWithSIGTERM delivers the binary's real shutdown signal and waits for
// run() to return. The test's own handler is registered FIRST so the
// process survives signals sent before run() reaches its signal.Notify,
// and the signal is retried for determinism (the skill's documented drain
// contract for binaries whose run() takes no context).
func stopWithSIGTERM(t *testing.T, done <-chan error) error {
	t.Helper()
	watch := make(chan os.Signal, 1)
	signal.Notify(watch, syscall.SIGTERM)
	defer signal.Stop(watch)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		select {
		case err := <-done:
			return err
		case <-watch:
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatal("run() did not return after SIGTERM")
	return nil
}

// laborRowDTO mirrors the documented LaborRow wire schema
// (apis/openapi-reports.yaml); only the fields this test asserts on.
type laborRowDTO struct {
	PathId        string `json:"pathId"`
	HourBucket    string `json:"hourBucket"`
	ShiftsStarted int    `json:"shiftsStarted"`
	LaborAssigned int    `json:"laborAssigned"`
}

type laborReportDTO struct {
	Rows []laborRowDTO `json:"rows"`
}

// TestReportsBootServeTheSeededProjectionAndStopCleanly: real run() over an
// analytically-migrated empty database seeded through the repo's own writer.
func TestReportsBootServeTheSeededProjectionAndStopCleanly(t *testing.T) {
	url := startEmptyAnalyticsDB(t)

	// Seed the read model the documented way BEFORE the reader boots: the
	// reports binary opens the database read-only, so facts come from the
	// writer's own projection (analyticsstore's integration suite pattern).
	ctx := context.Background()
	writer, err := analyticsstore.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("open writer pool: %v", err)
	}
	defer writer.Close()
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	projection := analyticsstore.NewPostgresProjection(writer)
	if err := projection.ApplyShiftStarted(ctx, "reports-itest-ss-1", "assoc-rep-1", at); err != nil {
		t.Fatalf("seed shift started: %v", err)
	}
	if err := projection.ApplyLaborAssigned(ctx, "reports-itest-la-1", "PACK", at); err != nil {
		t.Fatalf("seed labor assigned: %v", err)
	}

	addr := freeAddr(t)
	t.Setenv("ANALYTICS_DATABASE_URL", url)
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("LOG_LEVEL", "error")

	done := make(chan error, 1)
	go func() { done <- run() }()

	waitUntil(t, "/healthz 200", func() bool {
		code, _, _ := get(t, "http://"+addr+"/healthz")
		return code == http.StatusOK
	})

	// The documented report: both buckets of the seeded hour — the
	// building-wide empty path (shift start) and PACK (assignment).
	code, _, raw := get(t, "http://"+addr+"/reports/labor?from=2026-11-13T09:00:00Z&to=2026-11-13T10:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("/reports/labor = %d %s", code, raw)
	}
	var report laborReportDTO
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("/reports/labor body is not JSON: %v: %s", err, raw)
	}
	rows := map[string]laborRowDTO{}
	for _, r := range report.Rows {
		rows[r.PathId] = r
	}
	if len(report.Rows) != 2 || rows[""].ShiftsStarted != 1 || rows["PACK"].LaborAssigned != 1 {
		t.Fatalf("/reports/labor rows = %+v, want one empty-path row with shiftsStarted=1 and one PACK row with laborAssigned=1", report.Rows)
	}

	// The documented optional pathId exact-match filter narrows to PACK.
	code, _, raw = get(t, "http://"+addr+"/reports/labor?from=2026-11-13T09:00:00Z&to=2026-11-13T10:00:00Z&pathId=PACK")
	if code != http.StatusOK {
		t.Fatalf("/reports/labor?pathId=PACK = %d %s", code, raw)
	}
	report = laborReportDTO{}
	if err := json.Unmarshal([]byte(raw), &report); err != nil || len(report.Rows) != 1 || report.Rows[0].PathId != "PACK" || report.Rows[0].LaborAssigned != 1 {
		t.Fatalf("/reports/labor?pathId=PACK = %s (%v)", raw, err)
	}

	// The documented freshness probe answers over the seeded events.
	code, _, raw = get(t, "http://"+addr+"/reports/labor/freshness")
	if code != http.StatusOK {
		t.Fatalf("/reports/labor/freshness = %d %s", code, raw)
	}
	var freshness struct {
		LagSeconds float64 `json:"lagSeconds"`
	}
	if err := json.Unmarshal([]byte(raw), &freshness); err != nil || freshness.LagSeconds < 0 {
		t.Fatalf("/reports/labor/freshness = %s (%v)", raw, err)
	}

	// The documented RFC 7807 400 for a malformed query (ADR-0037).
	code, contentType, raw := get(t, "http://"+addr+"/reports/labor")
	if code != http.StatusBadRequest {
		t.Fatalf("/reports/labor without from = %d %s, want 400", code, raw)
	}
	if contentType != "application/problem+json" {
		t.Fatalf("/reports/labor without from content-type = %q, want application/problem+json", contentType)
	}

	if err := stopWithSIGTERM(t, done); err != nil {
		t.Fatalf("a SIGTERM'd reports server must drain cleanly, got %v", err)
	}
}
