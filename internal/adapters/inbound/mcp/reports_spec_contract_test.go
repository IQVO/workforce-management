package mcp_test

// Consumer contract tests for the INTRA-REPO reports edge: the MCP reports
// REST client (ReportsRESTClient, wired from cmd/mcp's REPORTS_BASE_URL)
// calling this repo's own cmd/workforce-reports service, whose contract is
// apis/openapi-reports.yaml. The pin in testdata/ is a SELF-pin (producer and
// consumer are the same repo), registered in contracts/pinned.json against
// IQVO/workforce-management apis/openapi-reports.yaml@develop and kept equal
// by the pinned-contracts CI job: editing the spec without updating this
// client now fails CI instead of silently stranding the MCP tool.
//
// Archetypes as in the outbound contract tests: R1 route, R2 fixture, R3
// client, R4 DTO reflection. External _test package on purpose: the client
// and its view DTOs are exported. Fixture provenance: openapi-reports.yaml
// documents no whole-response examples, so both fixtures are assembled from
// the schemas' own per-property examples (LaborRow: pack / 2026-06-01T14:00:00Z
// / 3 / 0 / 2 / 300 / 1 / 12 / 3 / 1; Freshness: 4.2).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/claudioed/workforce-management/internal/adapters/inbound/mcp"
)

const specCopy = "testdata/workforce-management.openapi-reports.yaml"

func loadPinnedSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(specCopy)
	if err != nil {
		t.Fatalf("cannot load the pinned spec copy: %v", err)
	}
	return doc
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// R1: the client calls GET /reports/labor (GetLabor) and GET
// /reports/labor/freshness (GetFreshness); the pinned spec must still declare
// both, with the query parameters the client sends -- from/to required,
// pathId/granularity optional, granularity fixed to hour.
func TestSpecCoversTheCallSites(t *testing.T) {
	doc := loadPinnedSpec(t)

	labor := doc.Paths.Find("/reports/labor")
	if labor == nil || labor.Get == nil {
		t.Fatal("the pinned spec no longer declares GET /reports/labor")
	}
	if labor.Get.OperationID != "getLaborReport" {
		t.Fatalf("operationId = %q, want getLaborReport", labor.Get.OperationID)
	}
	params := map[string]*openapi3.Parameter{}
	for _, p := range labor.Get.Parameters {
		if p.Value != nil {
			params[p.Value.Name] = p.Value
		}
	}
	for _, q := range []struct {
		name     string
		required bool
	}{
		{"from", true},
		{"to", true},
		{"pathId", false},
		{"granularity", false},
	} {
		if params[q.name] == nil {
			t.Fatalf("/reports/labor no longer declares the %q query parameter", q.name)
		}
		if params[q.name].In != "query" || params[q.name].Required != q.required {
			t.Fatalf("%s: in=%s required=%v, want query/%v", q.name, params[q.name].In, params[q.name].Required, q.required)
		}
	}
	if schema := params["granularity"].Schema.Value; len(schema.Enum) == 0 {
		t.Fatal("granularity lost its enum; the client forwards the tool's value verbatim")
	}

	freshness := doc.Paths.Find("/reports/labor/freshness")
	if freshness == nil || freshness.Get == nil {
		t.Fatal("the pinned spec no longer declares GET /reports/labor/freshness")
	}
	if freshness.Get.OperationID != "getLaborReportFreshness" {
		t.Fatalf("operationId = %q, want getLaborReportFreshness", freshness.Get.OperationID)
	}
}

// R2: both wire fixtures satisfy the pinned LaborReport and Freshness schemas.
func TestFixturesSatisfyThePinnedSchemas(t *testing.T) {
	doc := loadPinnedSpec(t)
	for name, schema := range map[string]string{
		"labor-report.example.json": "LaborReport",
		"freshness.example.json":    "Freshness",
	} {
		var body any
		if err := json.Unmarshal(fixture(t, name), &body); err != nil {
			t.Fatal(err)
		}
		if err := doc.Components.Schemas[schema].Value.VisitJSON(body); err != nil {
			t.Errorf("%s violates %s: %v", name, schema, err)
		}
	}
}

// request records what the client actually sent.
type request struct {
	method, path, rawQuery string
}

// serveReports answers every request with status and body and records the last one.
func serveReports(t *testing.T, status int, body []byte) (*mcp.ReportsRESTClient, *request) {
	t.Helper()
	got := &request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.rawQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return mcp.NewReportsRESTClient(srv.URL, srv.Client()), got
}

// R3: the real client decodes the fixture over a real HTTP round trip and
// sends the window the spec requires, mapped onto the spec's query names.
func TestClientDecodesTheLaborFixture(t *testing.T) {
	c, got := serveReports(t, http.StatusOK, fixture(t, "labor-report.example.json"))
	report, err := c.GetLabor(t.Context(), mcp.LaborQuery{
		From: "2026-06-01T00:00:00Z", To: "2026-06-02T00:00:00Z", PathId: "pack", Granularity: "hour",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(report.Rows))
	}
	row := report.Rows[0]
	if row.PathId != "pack" || row.HourBucket != "2026-06-01T14:00:00Z" || row.ShiftsStarted != 3 ||
		row.LaborAssigned != 12 || row.UnderstaffingEvents != 1 {
		t.Fatalf("row = %+v", row)
	}
	q, qerr := url.ParseQuery(got.rawQuery)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if got.method != http.MethodGet || got.path != "/reports/labor" ||
		q.Get("from") != "2026-06-01T00:00:00Z" || q.Get("to") != "2026-06-02T00:00:00Z" ||
		q.Get("pathId") != "pack" || q.Get("granularity") != "hour" {
		t.Fatalf("request = %s %s %s", got.method, got.path, got.rawQuery)
	}
}

// R3 (freshness + failure): the freshness fixture decodes, and the spec's
// documented 400 problem+json / 500 responses surface as client errors.
func TestClientDecodesTheFreshnessFixtureAndFailsLoud(t *testing.T) {
	c, _ := serveReports(t, http.StatusOK, fixture(t, "freshness.example.json"))
	fresh, err := c.GetFreshness(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.LagSeconds != 4.2 {
		t.Fatalf("lagSeconds = %v, want 4.2", fresh.LagSeconds)
	}
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"problem json": {http.StatusBadRequest, `{"type":"https://errors.workforce-management.warehouse-systems.dev/invalid-report-query","title":"The report query is malformed or missing a required parameter","status":400,"detail":"query parameter 'from' is required (RFC3339)"}`},
		"store error":  {http.StatusInternalServerError, `{}`},
		"garbage":      {http.StatusOK, `not json`},
	} {
		c, _ := serveReports(t, tc.status, []byte(tc.body))
		if _, err := c.GetLabor(t.Context(), mcp.LaborQuery{From: "2026-06-01T00:00:00Z", To: "2026-06-02T00:00:00Z"}); err == nil {
			t.Errorf("%s: the client swallowed a %d response", name, tc.status)
		}
	}
}

// R4: every view DTO json tag is declared by the pinned schema and every
// schema-required field is mirrored, tolerant of producer-side additions the
// views do not (yet) mirror.
func TestViewDTOsMatchThePinnedSchemas(t *testing.T) {
	doc := loadPinnedSpec(t)
	for name, tc := range map[string]struct {
		dto    reflect.Type
		schema string
	}{
		"LaborRowView":    {reflect.TypeOf(mcp.LaborRowView{}), "LaborRow"},
		"LaborReportView": {reflect.TypeOf(mcp.LaborReportView{}), "LaborReport"},
		"FreshnessView":   {reflect.TypeOf(mcp.FreshnessView{}), "Freshness"},
	} {
		schema := doc.Components.Schemas[tc.schema].Value
		tags := map[string]bool{}
		for i := range tc.dto.NumField() {
			tag := strings.Split(tc.dto.Field(i).Tag.Get("json"), ",")[0]
			if tag != "" && tag != "-" {
				tags[tag] = true
			}
		}
		for tag := range tags {
			if _, declared := schema.Properties[tag]; !declared {
				t.Errorf("%s mirrors %q, which %s does not declare", name, tag, tc.schema)
			}
		}
		for _, required := range schema.Required {
			if _, mirrored := tags[required]; !mirrored {
				t.Errorf("%s requires %q, which %s does not mirror", tc.schema, required, name)
			}
		}
	}
}
