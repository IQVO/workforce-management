package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Convergence on siteCode (decision 19, ADR 0035): siteCode is the canonical
// name of the ShiftPlan key, buildingId its deprecated alias (same value,
// same stored column).

func commitLines() []pathPlanLineRequest {
	return []pathPlanLineRequest{{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40, InstalledStations: 10}}
}

func decodeObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return got
}

func TestCommitShiftPlan_SiteCodeAndDeprecatedBuildingIdAlias(t *testing.T) {
	tests := []struct {
		name       string
		siteCode   string
		buildingId string
		wantStatus int
		wantKey    string // the value both response fields carry
		wantSlug   string
	}{
		{"siteCode only", "WH1", "", http.StatusCreated, "WH1", ""},
		{"buildingId only (deprecated alias)", "", "bldg-1", http.StatusCreated, "bldg-1", ""},
		{"both present and equal", "WH1", "WH1", http.StatusCreated, "WH1", ""},
		{"both present and different", "WH1", "bldg-1", http.StatusUnprocessableEntity, "", "conflicting-site-and-building"},
		{"neither", "", "", http.StatusBadRequest, "", "missing-building-id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouter(newTestHandler(), testLogger, "")
			rec := doRequest(t, router, http.MethodPost, "/shift-plans", commitShiftPlanRequest{
				SiteCode: tc.siteCode, BuildingId: tc.buildingId, ShiftId: "shift-1", Lines: commitLines(),
			})
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantSlug != "" {
				assertProblemDetails(t, rec, tc.wantStatus, tc.wantSlug, "/shift-plans")
				return
			}
			got := decodeObject(t, rec)
			if got["siteCode"] != tc.wantKey || got["buildingId"] != tc.wantKey {
				t.Fatalf("response must carry siteCode AND buildingId with the same value %q, got %v", tc.wantKey, got)
			}
			if loc := rec.Header().Get("Location"); loc != "/shift-plans/"+tc.wantKey+"/shift-1" {
				t.Fatalf("Location = %q, want /shift-plans/%s/shift-1", loc, tc.wantKey)
			}
		})
	}
}

func TestProposePathPlan_SiteCodeAndDeprecatedBuildingIdAlias(t *testing.T) {
	tests := []struct {
		name       string
		siteCode   string
		buildingId string
		wantStatus int
		wantSlug   string
	}{
		{"siteCode only", "WH1", "", http.StatusOK, ""},
		{"buildingId only (deprecated alias)", "", "bldg-1", http.StatusOK, ""},
		{"both present and equal", "WH1", "WH1", http.StatusOK, ""},
		{"both present and different", "WH1", "bldg-1", http.StatusUnprocessableEntity, "conflicting-site-and-building"},
		{"neither", "", "", http.StatusBadRequest, "missing-building-id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouter(newTestHandler(), testLogger, "")
			rec := doRequest(t, router, http.MethodPost, "/paths/pack/plan/propose", proposePathPlanRequest{
				SiteCode: tc.siteCode, BuildingId: tc.buildingId, Charge: chargePtr(100), PlannedRate: 30,
			})
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantSlug != "" {
				assertProblemDetails(t, rec, tc.wantStatus, tc.wantSlug, "/paths/pack/plan/propose")
			}
		})
	}
}

// A plan committed through the deprecated buildingId is readable through
// siteCode, and the reverse -- they are one stored key.
func TestStaffingGap_PlanWrittenViaOneNameIsReadableViaTheOther(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	for _, c := range []struct{ siteCode, buildingId, shift string }{
		{"", "legacy-bldg", "s-legacy"}, // written via the deprecated alias
		{"WH9", "", "s-site"},           // written via the canonical name
	} {
		rec := doRequest(t, router, http.MethodPost, "/shift-plans", commitShiftPlanRequest{
			SiteCode: c.siteCode, BuildingId: c.buildingId, ShiftId: c.shift, Lines: commitLines(),
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("commit %+v: %d %s", c, rec.Code, rec.Body.String())
		}
	}

	// legacy-written, read via the canonical siteCode query parameter
	got := decodeGap(t, doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?siteCode=legacy-bldg&shiftId=s-legacy", nil))
	if got["plannedHeads"] != float64(5) || got["siteCode"] != "legacy-bldg" {
		t.Fatalf("plan written via buildingId must be readable via siteCode, got %v", got)
	}
	// site-written, read via the deprecated buildingId query parameter
	got = decodeGap(t, doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?buildingId=WH9&shiftId=s-site", nil))
	if got["plannedHeads"] != float64(5) {
		t.Fatalf("plan written via siteCode must be readable via buildingId, got %v", got)
	}
	if _, scoped := got["siteCode"]; scoped {
		t.Fatalf("a legacy buildingId-only call is NOT auto-scoped by site, got %v", got)
	}
}

func TestStaffingGap_ByPath_SiteCodeIsPlanKeyAndScope(t *testing.T) {
	router := siteFixtureAtSite(t)
	tests := []struct {
		name       string
		query      string
		wantActive float64
		wantSite   any // nil = key absent (unscoped)
	}{
		{"siteCode only: plan lookup AND scope", "siteCode=WH1&shiftId=shift-1", 2, "WH1"},
		{"buildingId only: plan lookup, NOT scoped (identical to before)", "buildingId=WH1&shiftId=shift-1", 4, nil},
		{"both equal", "siteCode=WH1&buildingId=WH1&shiftId=shift-1", 2, "WH1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeGap(t, doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?"+tc.query, nil))
			if got["activeHeads"] != tc.wantActive {
				t.Fatalf("activeHeads = %v, want %v: %v", got["activeHeads"], tc.wantActive, got)
			}
			if got["siteCode"] != tc.wantSite {
				t.Fatalf("siteCode = %v, want %v: %v", got["siteCode"], tc.wantSite, got)
			}
		})
	}

	t.Run("neither", func(t *testing.T) {
		rec := doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?shiftId=shift-1", nil)
		assertProblemDetails(t, rec, http.StatusBadRequest, "missing-building-id", "/paths/pack/staffing-gap")
	})
}

// siteFixtureAtSite is siteFixture with the plan keyed by the Site code WH1:
// two packers at WH1, one at WH2, one without a site; plan = 4.
func siteFixtureAtSite(t *testing.T) http.Handler {
	t.Helper()
	router := NewRouter(newTestHandler(), testLogger, "")
	startShiftAt(t, router, "wh1-a", "WH1")
	startShiftAt(t, router, "wh1-b", "WH1")
	startShiftAt(t, router, "wh2-a", "WH2")
	startShiftAt(t, router, "legacy-a", "")
	for _, id := range []string{"wh1-a", "wh1-b", "wh2-a", "legacy-a"} {
		assignPack(t, router, id)
	}
	rec := doRequest(t, router, http.MethodPost, "/shift-plans", commitShiftPlanRequest{
		SiteCode: "WH1", ShiftId: "shift-1",
		Lines: []pathPlanLineRequest{{PathId: "pack", PlannedHeads: 4, PlannedRate: 30, PlannedHours: 8, InstalledStations: 10}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("commit plan: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	return router
}

// The canonical GET /sites/{siteCode}/shifts/{shiftId}/staffing-gap and the
// deprecated GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap share
// one handler and one response shape; only the old route announces its
// deprecation (Deprecation: true) and it stays unscoped (identical to before).
func TestStaffingGapForShift_CanonicalSitesRouteAndDeprecatedBuildingsRoute(t *testing.T) {
	router := siteFixtureAtSite(t)
	assertCanonicalSitesRoute(t, router)
	assertDeprecatedBuildingsRoute(t, router)
	assertSiteRouteNotFounds(t, router)
}

// assertCanonicalSitesRoute: the canonical route is not deprecated and uses the
// one siteCode as both plan lookup key and associate scope.
func assertCanonicalSitesRoute(t *testing.T, router http.Handler) {
	t.Helper()
	canonical := doRequest(t, router, http.MethodGet, "/sites/WH1/shifts/shift-1/staffing-gap", nil)
	if canonical.Code != http.StatusOK {
		t.Fatalf("canonical route: %d %s", canonical.Code, canonical.Body.String())
	}
	if h := canonical.Header().Get("Deprecation"); h != "" {
		t.Fatalf("the canonical route must not be marked deprecated, got Deprecation: %q", h)
	}
	var sites []map[string]any
	if err := json.Unmarshal(canonical.Body.Bytes(), &sites); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sites) != 1 || sites[0]["activeHeads"] != float64(2) || sites[0]["siteCode"] != "WH1" || sites[0]["pathId"] != "pack" {
		t.Fatalf("canonical route: plan lookup AND scope by the one siteCode, got %v", sites)
	}
}

// assertDeprecatedBuildingsRoute: the legacy route keeps working, announces its
// deprecation, and (buildingId only) is not auto-scoped by site.
func assertDeprecatedBuildingsRoute(t *testing.T, router http.Handler) {
	t.Helper()
	legacy := doRequest(t, router, http.MethodGet, "/buildings/WH1/shifts/shift-1/staffing-gap", nil)
	if legacy.Code != http.StatusOK {
		t.Fatalf("deprecated route must keep working: %d %s", legacy.Code, legacy.Body.String())
	}
	if h := legacy.Header().Get("Deprecation"); h != "true" {
		t.Fatalf("the deprecated route must answer Deprecation: true, got %q", h)
	}
	var buildings []map[string]any
	if err := json.Unmarshal(legacy.Body.Bytes(), &buildings); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(buildings) != 1 || buildings[0]["activeHeads"] != float64(4) {
		t.Fatalf("the legacy buildingId-only route is not auto-scoped (fleet-wide), got %v", buildings)
	}
	if _, scoped := buildings[0]["siteCode"]; scoped {
		t.Fatalf("an unscoped entry must omit siteCode, got %v", buildings[0])
	}
}

// assertSiteRouteNotFounds: the deprecation header is also on the legacy route's
// error responses, and the canonical route's 404 is the same problem+json.
func assertSiteRouteNotFounds(t *testing.T, router http.Handler) {
	t.Helper()
	missing := doRequest(t, router, http.MethodGet, "/buildings/nope/shifts/shift-1/staffing-gap", nil)
	if missing.Code != http.StatusNotFound || missing.Header().Get("Deprecation") != "true" {
		t.Fatalf("deprecated route 404 must still carry Deprecation: true, got %d %q", missing.Code, missing.Header().Get("Deprecation"))
	}
	notFound := doRequest(t, router, http.MethodGet, "/sites/NOPE/shifts/shift-1/staffing-gap", nil)
	assertProblemDetails(t, notFound, http.StatusNotFound, "resource-not-found", "/sites/NOPE/shifts/shift-1/staffing-gap")
}
