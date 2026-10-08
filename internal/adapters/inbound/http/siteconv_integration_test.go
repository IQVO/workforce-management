//go:build integration

// Integration test for the siteCode convergence (decision 19, ADR 0035)
// against a real Postgres 16 (testcontainers only, never DATABASE_URL), through
// the REAL chi router: a ShiftPlan written under the deprecated buildingId is
// readable under siteCode, and the reverse -- because both are ONE stored
// column (shift_plan.building_id, no migration) -- and the outbox rows carry
// site_code next to building_id with the same value.
package http_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	inboundhttp "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
)

func siteConvRouter(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	shiftPlans := postgres.NewShiftPlanRepo(pool)
	assignments := postgres.NewAssignmentRepo(pool)
	uow := postgres.NewUnitOfWork(pool)
	outbox := postgres.NewOutboxPublisher(pool,
		outboundkafka.NewPublisherWithWriter(nil, shiftPlans),
		outboundkafka.NewAnalyticsPublisherWithWriter(nil, outboundkafka.NewEventID))
	clock := idemClock(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	catalogue := pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
	})
	h := &inboundhttp.Handler{
		CommitShiftPlan: &usecases.CommitShiftPlan{
			ShiftPlans: shiftPlans, Events: outbox, Clock: clock, InstalledCapacity: unlimitedCapacity{},
			Catalogue: catalogue, MaxHoursPerShift: 8, UnitOfWork: uow,
		},
		GetStaffingGap: &usecases.GetStaffingGap{
			ShiftPlans: shiftPlans, Assignments: assignments, Events: outbox, Clock: clock, UnitOfWork: uow,
		},
		Catalogue:       catalogue,
		IdempotencyPool: pool,
	}
	return inboundhttp.NewRouter(h, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
}

func get(router http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestSiteConvergence_PlanWrittenViaOneNameIsReadableViaTheOther(t *testing.T) {
	pool := idempotencyDB(t)
	router := siteConvRouter(t, pool)
	line := `"lines":[{"pathId":"pack","plannedHeads":5,"plannedRate":30,"plannedHours":40,"installedStations":10}]`

	// Written via the DEPRECATED alias.
	rec := post(router, "/shift-plans", "sc-1", `{"buildingId":"LEGACY-1","shiftId":"S1",`+line+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("commit via buildingId: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created["siteCode"] != "LEGACY-1" || created["buildingId"] != "LEGACY-1" {
		t.Fatalf("response must carry both names with one value, got %v", created)
	}

	// Written via the CANONICAL name.
	rec = post(router, "/shift-plans", "sc-2", `{"siteCode":"WH7","shiftId":"S1",`+line+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("commit via siteCode: %d %s", rec.Code, rec.Body.String())
	}

	// One stored column holds both: no migration, no second column.
	if got := count(t, pool, `SELECT count(*) FROM shift_plan WHERE building_id IN ('LEGACY-1','WH7')`); got != 2 {
		t.Fatalf("both plans must live in shift_plan.building_id, found %d", got)
	}

	// buildingId-written plan, read by siteCode (canonical route and by-path query).
	for _, path := range []string{
		"/sites/LEGACY-1/shifts/S1/staffing-gap",
		"/buildings/LEGACY-1/shifts/S1/staffing-gap",
	} {
		r := get(router, path)
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"plannedHeads":5`) {
			t.Fatalf("GET %s: %d %s", path, r.Code, r.Body.String())
		}
	}
	// siteCode-written plan, read by the deprecated buildingId.
	for _, path := range []string{
		"/buildings/WH7/shifts/S1/staffing-gap",
		"/sites/WH7/shifts/S1/staffing-gap",
	} {
		r := get(router, path)
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"plannedHeads":5`) {
			t.Fatalf("GET %s: %d %s", path, r.Code, r.Body.String())
		}
	}

	// The deprecated route announces its deprecation; the canonical one does not.
	if got := get(router, "/buildings/WH7/shifts/S1/staffing-gap").Header().Get("Deprecation"); got != "true" {
		t.Fatalf("Deprecation = %q, want true", got)
	}
	if got := get(router, "/sites/WH7/shifts/S1/staffing-gap").Header().Get("Deprecation"); got != "" {
		t.Fatalf("canonical route must not be deprecated, got %q", got)
	}

	// The outbox carries site_code AND building_id with the same value, and the
	// Kafka key / subject (<site>/<shift>) did not change.
	var key, value string
	if err := pool.QueryRow(context.Background(),
		`SELECT convert_from(key,'UTF8'), convert_from(value,'UTF8') FROM outbox_events
		  WHERE topic = 'warehouse.workforce.events' AND convert_from(key,'UTF8') = 'WH7/S1' LIMIT 1`).Scan(&key, &value); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	for _, want := range []string{`"site_code":"WH7"`, `"building_id":"WH7"`, `"subject":"WH7/S1"`} {
		if !strings.Contains(value, want) {
			t.Fatalf("integration outbox value must contain %s, got %s", want, value)
		}
	}
}

func TestSiteConvergence_ConflictingNamesAreRejectedAndNothingIsWritten(t *testing.T) {
	pool := idempotencyDB(t)
	router := siteConvRouter(t, pool)
	rec := post(router, "/shift-plans", "sc-conflict",
		`{"siteCode":"WH7","buildingId":"LEGACY-1","shiftId":"S1","lines":[{"pathId":"pack","plannedHeads":5,"plannedRate":30,"plannedHours":40,"installedStations":10}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if slug := problemSlug(t, rec.Body.Bytes()); slug != "conflicting-site-and-building" {
		t.Fatalf("slug = %q", slug)
	}
	if got := count(t, pool, "SELECT count(*) FROM shift_plan"); got != 0 {
		t.Fatalf("a rejected commit must write nothing, found %d plans", got)
	}
}
