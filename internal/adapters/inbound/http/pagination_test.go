package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

type gapPage struct {
	Items      []staffingGapResponse `json:"items"`
	NextCursor string                `json:"nextCursor"`
}

// Walks every page of the all-paths staffing-gap list with a limit smaller
// than the plan: no duplicate and no skipped path, last page has no cursor.
func TestStaffingGapPages_WalkEveryPageNoDuplicateNoSkip(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	doRequest(t, router, http.MethodPost, "/shift-plans", commitShiftPlanRequest{
		BuildingId: "WH1", ShiftId: "S1",
		Lines: []pathPlanLineRequest{
			{PathId: "pick", PlannedHeads: 1, PlannedRate: 25, PlannedHours: 8, InstalledStations: 10},
			{PathId: "pack", PlannedHeads: 1, PlannedRate: 25, PlannedHours: 8, InstalledStations: 10},
			{PathId: "rebin", PlannedHeads: 1, PlannedRate: 25, PlannedHours: 8, InstalledStations: 10},
			{PathId: "slam", PlannedHeads: 1, PlannedRate: 25, PlannedHours: 8, InstalledStations: 10},
			{PathId: "pack-zone-a", PlannedHeads: 1, PlannedRate: 25, PlannedHours: 8, InstalledStations: 10},
		},
	})
	for _, base := range []string{"/sites/WH1/shifts/S1/staffing-gap", "/buildings/WH1/shifts/S1/staffing-gap"} {
		paths, pages := walkGapPages(t, router, base, 2)
		seen := map[string]bool{}
		for _, id := range paths {
			if seen[id] {
				t.Fatalf("%s: duplicate path %s", base, id)
			}
			seen[id] = true
		}
		if len(seen) != 5 || pages != 3 {
			t.Fatalf("%s: saw %d paths in %d pages, want 5 in 3", base, len(seen), pages)
		}
	}
}

// walkGapPages follows nextCursor to the end and returns every pathId seen.
func walkGapPages(t *testing.T, router http.Handler, base string, limit int) (ids []string, pages int) {
	t.Helper()
	cursor := ""
	for {
		target := fmt.Sprintf("%s?limit=%d", base, limit)
		if cursor != "" {
			target += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := doRequest(t, router, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
		var p gapPage
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		pages++
		for _, g := range p.Items {
			ids = append(ids, g.PathId)
		}
		if p.NextCursor == "" {
			return ids, pages
		}
		cursor = p.NextCursor
	}
}

func TestStaffingGapPages_BadParamsAre400(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	cases := []struct{ query, slug string }{
		{"?limit=0", "invalid-limit"},
		{"?limit=501", "invalid-limit"},
		{"?limit=", "invalid-limit"},
		{"?limit=1&limit=2", "invalid-limit"},
		{"?cursor=%21%21", "invalid-cursor"},
		{"?cursor=a&cursor=b", "invalid-cursor"},
		{"?after=x", "unknown-query-parameter"},
		{"?offset=1", "unknown-query-parameter"},
		{"?nextToken=x", "unknown-query-parameter"},
		{"?bogus=1", "unknown-query-parameter"},
	}
	for _, base := range []string{"/sites/WH1/shifts/S1/staffing-gap", "/buildings/WH1/shifts/S1/staffing-gap"} {
		for _, tc := range cases {
			rec := doRequest(t, router, http.MethodGet, base+tc.query, nil)
			assertProblemDetails(t, rec, http.StatusBadRequest, tc.slug, base)
		}
	}
}

// decodeItems reads the {items:[...]} envelope of a collection answer.
func decodeItems(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Items == nil {
		t.Fatalf("decode {items}: %v: %s", err, body)
	}
	return env.Items
}
