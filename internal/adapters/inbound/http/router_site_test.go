package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Site-scoped staffing gap over HTTP (decision 6, ADR 0034).

func startShiftAt(t *testing.T, router http.Handler, id, site string) {
	t.Helper()
	rec := doRequest(t, router, http.MethodPost, "/associates/"+id+"/start-shift", startShiftRequest{Certifications: []string{"pack"}, SiteCode: site})
	if rec.Code != http.StatusCreated {
		t.Fatalf("start-shift %s: expected 201, got %d: %s", id, rec.Code, rec.Body.String())
	}
}

func assignPack(t *testing.T, router http.Handler, id string) {
	t.Helper()
	rec := doRequest(t, router, http.MethodPost, "/associates/"+id+"/assignments", assignLaborRequest{PathId: "pack"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("assign %s: expected 201, got %d: %s", id, rec.Code, rec.Body.String())
	}
}

// siteFixture: two packers at WH1, one at WH2, one with no site; plan = 4.
func siteFixture(t *testing.T) http.Handler {
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
		BuildingId: "bldg-1", ShiftId: "shift-1",
		Lines: []pathPlanLineRequest{{PathId: "pack", PlannedHeads: 4, PlannedRate: 30, PlannedHours: 8, InstalledStations: 10}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("commit plan: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	return router
}

func decodeGap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestStartShift_AcceptsOptionalSiteCode(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", map[string]any{"certifications": []string{"pack"}, "siteCode": "WH1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The spec declares siteCode nullable (like certifications): an explicit JSON
// null is the same as omitting it, so the site stays unknown (contract job).
func TestStartShift_NullSiteCodeIsOmitted(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", map[string]any{"certifications": []string{"pack"}, "siteCode": nil})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	assignPack(t, router, "assoc-1")
	rec = doRequest(t, router, http.MethodPost, "/shift-plans", commitShiftPlanRequest{
		BuildingId: "bldg-1", ShiftId: "shift-1",
		Lines: []pathPlanLineRequest{{PathId: "pack", PlannedHeads: 1, PlannedRate: 30, PlannedHours: 8, InstalledStations: 10}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("commit plan: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	scoped := decodeGap(t, doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1&siteCode=WH1", nil))
	if scoped["activeHeads"] != float64(0) {
		t.Fatalf("a null siteCode must leave the associate site-unknown (not counted at WH1), got %v", scoped)
	}
}

func TestStaffingGap_SingleUnscopedStaysFleetWide(t *testing.T) {
	router := siteFixture(t)
	got := decodeGap(t, doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1", nil))
	if got["activeHeads"] != float64(4) || got["understaffed"] != false {
		t.Fatalf("unscoped must count every site and the legacy associate, got %v", got)
	}
	if _, present := got["siteCode"]; present {
		t.Fatalf("an unscoped response must omit siteCode, got %v", got)
	}
}

func TestStaffingGap_SingleScopedCountsOnlyThatSite(t *testing.T) {
	router := siteFixture(t)

	wh1 := decodeGap(t, doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1&siteCode=WH1", nil))
	if wh1["activeHeads"] != float64(2) || wh1["understaffed"] != true || wh1["siteCode"] != "WH1" {
		t.Fatalf("WH1 expected 2 active heads, understaffed, siteCode echoed; got %v", wh1)
	}

	wh2 := decodeGap(t, doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1&siteCode=WH2", nil))
	if wh2["activeHeads"] != float64(1) || wh2["siteCode"] != "WH2" {
		t.Fatalf("WH2 expected 1 active head; got %v", wh2)
	}

	unknown := decodeGap(t, doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1&siteCode=NOPE", nil))
	if unknown["activeHeads"] != float64(0) || unknown["understaffed"] != true {
		t.Fatalf("a site nobody works at counts 0 (code accepted as given); got %v", unknown)
	}
}

func TestStaffingGap_BlankSiteCodeIsUnscoped(t *testing.T) {
	router := siteFixture(t)
	got := decodeGap(t, doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1&siteCode=", nil))
	if got["activeHeads"] != float64(4) {
		t.Fatalf("an empty siteCode must behave as unscoped, got %v", got)
	}
}

func TestStaffingGapForShift_ScopedList(t *testing.T) {
	router := siteFixture(t)

	rec := doRequest(t, router, http.MethodGet, "/buildings/bldg-1/shifts/shift-1/staffing-gap?siteCode=WH1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var scoped []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &scoped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(scoped) != 1 || scoped[0]["activeHeads"] != float64(2) || scoped[0]["siteCode"] != "WH1" {
		t.Fatalf("scoped list expected one pack line with 2 heads at WH1, got %v", scoped)
	}

	rec = doRequest(t, router, http.MethodGet, "/buildings/bldg-1/shifts/shift-1/staffing-gap", nil)
	var all []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(all) != 1 || all[0]["activeHeads"] != float64(4) {
		t.Fatalf("unscoped list must stay fleet-wide, got %v", all)
	}
	if _, present := all[0]["siteCode"]; present {
		t.Fatalf("an unscoped list entry must omit siteCode, got %v", all[0])
	}
}
