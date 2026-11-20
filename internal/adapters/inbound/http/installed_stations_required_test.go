package http

import (
	"encoding/json"
	"net/http"
	"testing"
)

// intp returns a pointer to i: pathPlanLineRequest.InstalledStations is a
// pointer so an omitted count is distinguishable from an explicit 0.
func intp(i int) *int { return &i }

// TestCommitShiftPlan_InstalledStationsIsRequired pins the openapi contract
// (PathPlanLineRequest.required + POST /shift-plans 400): a line that omits
// installedStations, or sends null, is 400 shift-plan-missing-installed-stations
// -- never the 409 planned-heads-exceed-installed an omitted number decoded as 0
// used to produce. An EXPLICIT 0 is a stated count and is still checked against
// plannedHeads.
func TestCommitShiftPlan_InstalledStationsIsRequired(t *testing.T) {
	const prefix = `{"buildingId":"bldg-1","shiftId":"shift-1","lines":[`
	cases := []struct {
		name       string
		lines      string
		wantStatus int
		wantSlug   string
	}{
		{"omitted", `{"pathId":"pack","plannedHeads":2,"plannedRate":30,"plannedHours":16}`, http.StatusBadRequest, "shift-plan-missing-installed-stations"},
		{"null", `{"pathId":"pack","plannedHeads":2,"plannedRate":30,"plannedHours":16,"installedStations":null}`, http.StatusBadRequest, "shift-plan-missing-installed-stations"},
		{"omitted on the second line only",
			`{"pathId":"pack","plannedHeads":2,"plannedRate":30,"plannedHours":16,"installedStations":10},` +
				`{"pathId":"pick","plannedHeads":1,"plannedRate":25,"plannedHours":8}`,
			http.StatusBadRequest, "shift-plan-missing-installed-stations"},
		{"explicit zero is a stated count", `{"pathId":"pack","plannedHeads":2,"plannedRate":30,"plannedHours":16,"installedStations":0}`, http.StatusConflict, "planned-heads-exceed-installed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouter(newTestHandler(), testLogger, "")
			rec := doRequest(t, router, http.MethodPost, "/shift-plans", json.RawMessage(prefix+tc.lines+`]}`))
			if rec.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
			assertProblemDetails(t, rec, tc.wantStatus, tc.wantSlug, "/shift-plans")
		})
	}
}
