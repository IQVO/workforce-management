package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- reports REST views (tool + client boundary) ------------------------------

// LaborRowView is one row of the labor report as the MCP tool returns it and
// the reports REST client decodes it. Field tags match the reports service's
// JSON so the same struct round-trips both ways. An empty pathId is the
// building-wide, associate-scoped bucket.
type LaborRowView struct {
	PathId              string  `json:"pathId"`
	HourBucket          string  `json:"hourBucket"`
	ShiftsStarted       int     `json:"shiftsStarted"`
	ShiftsEnded         int     `json:"shiftsEnded"`
	Breaks              int     `json:"breaks"`
	AvgBreakSeconds     float64 `json:"avgBreakSeconds"`
	Certifications      int     `json:"certifications"`
	LaborAssigned       int     `json:"laborAssigned"`
	LaborReassigned     int     `json:"laborReassigned"`
	UnderstaffingEvents int     `json:"understaffingEvents"`
}

// LaborReportView is the labor report body.
type LaborReportView struct {
	Rows []LaborRowView `json:"rows"`
}

// FreshnessView is the freshness-lag body.
type FreshnessView struct {
	LagSeconds float64 `json:"lagSeconds"`
}

// LaborQuery is the filter set passed to the reports REST client.
type LaborQuery struct {
	From        string
	To          string
	PathId      string
	Granularity string
}

// ReportsClient is the narrow port the MCP report tool depends on: a client of
// the workforce-reports REST service. It is an interface so the tool can be
// unit-tested with a fake, and so the curated tool never talks to the
// analytical database directly — it goes through the reports REST surface,
// preserving the single read path (ADR-0010).
type ReportsClient interface {
	GetLabor(ctx context.Context, q LaborQuery) (LaborReportView, error)
	GetFreshness(ctx context.Context) (FreshnessView, error)
}

// --- reports REST client ------------------------------------------------------

// ReportsRESTClient is the HTTP implementation of ReportsClient. Base URL and
// the *http.Client are injected so the composition root controls the target and
// timeouts, and tests can point it at an httptest server.
type ReportsRESTClient struct {
	baseURL string
	http    *http.Client
}

// NewReportsRESTClient constructs a ReportsRESTClient for the reports service
// at baseURL. A nil httpClient falls back to a client with a sane timeout.
func NewReportsRESTClient(baseURL string, httpClient *http.Client) *ReportsRESTClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &ReportsRESTClient{baseURL: baseURL, http: httpClient}
}

// GetLabor calls GET /reports/labor with q as the query string.
func (c *ReportsRESTClient) GetLabor(ctx context.Context, q LaborQuery) (LaborReportView, error) {
	vals := url.Values{}
	vals.Set("from", q.From)
	vals.Set("to", q.To)
	if q.PathId != "" {
		vals.Set("pathId", q.PathId)
	}
	if q.Granularity != "" {
		vals.Set("granularity", q.Granularity)
	}
	var out LaborReportView
	if err := c.getJSON(ctx, "/reports/labor?"+vals.Encode(), &out); err != nil {
		return LaborReportView{}, err
	}
	return out, nil
}

// GetFreshness calls GET /reports/labor/freshness.
func (c *ReportsRESTClient) GetFreshness(ctx context.Context) (FreshnessView, error) {
	var out FreshnessView
	if err := c.getJSON(ctx, "/reports/labor/freshness", &out); err != nil {
		return FreshnessView{}, err
	}
	return out, nil
}

// getJSON performs a GET against baseURL+path and decodes a 2xx JSON body into
// out. A non-2xx response is an error.
func (c *ReportsRESTClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("reports client: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reports client: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("reports client: unexpected status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("reports client: decode: %w", err)
	}
	return nil
}

// Compile-time assertion that ReportsRESTClient satisfies the port.
var _ ReportsClient = (*ReportsRESTClient)(nil)

// --- get_workforce_labor_report tool ------------------------------------------

// LaborToolInput is the tool's argument set (untrusted, from a model).
type LaborToolInput struct {
	From        string `json:"from" jsonschema:"start of the window, inclusive, RFC3339 (required)"`
	To          string `json:"to" jsonschema:"end of the window, exclusive, RFC3339 (required)"`
	PathId      string `json:"pathId,omitempty" jsonschema:"optional process-path filter (e.g. pack, pick, stow); empty rows are the building-wide associate-scoped bucket"`
	Granularity string `json:"granularity,omitempty" jsonschema:"optional time bucket granularity; only 'hour' is supported, and omitting it means hour"`
}

// getLaborReport is the tool handler: it validates the required window,
// delegates to the reports REST client, and returns the report view.
func (d Deps) getLaborReport(ctx context.Context, in LaborToolInput) (LaborReportView, error) {
	return GetLaborReportForTest(ctx, d.Reports, in)
}

// GetLaborReportForTest is the tool's pure logic, factored out so it can be
// unit-tested with a fake ReportsClient independent of the MCP server wiring.
// It validates from/to and forwards the filters.
func GetLaborReportForTest(ctx context.Context, client ReportsClient, in LaborToolInput) (LaborReportView, error) {
	if client == nil {
		return LaborReportView{}, fmt.Errorf("reports client not configured")
	}
	if in.From == "" || in.To == "" {
		return LaborReportView{}, fmt.Errorf("from and to are required (RFC3339)")
	}
	q := LaborQuery{}
	q.From = in.From
	q.To = in.To
	q.PathId = in.PathId
	q.Granularity = in.Granularity
	return client.GetLabor(ctx, q)
}

// registerReportTool adds the curated read-only labor report tool. It is
// registered only when a reports client is configured (Deps.Reports != nil), so
// an MCP deployment without the reports service simply does not expose it.
func (d Deps) registerReportTool(server *mcp.Server) {
	if d.Reports == nil {
		return
	}
	readOnly := true
	addTool(server, &mcp.Tool{
		Name:        "get_workforce_labor_report",
		Description: "Return the workforce Labor Utilization & Staffing report (shifts started/ended, break count and avg break seconds, certifications, labor assigned/reassigned, understaffing events) for a time window, optionally filtered by process path. Reads via the workforce-reports REST service.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly},
	}, d.getLaborReport)
}
