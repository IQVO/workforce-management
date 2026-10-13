package mcp_test

import (
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestLaborReportTool_OptionalFiltersMayBeOmitted pins gap fix D16: pathId
// and granularity are optional filters (the REST endpoint defaults them), so
// a call that sends only the required window must pass the SDK's input
// schema validation and reach the reports client. Without `omitempty` the
// SDK infers both as REQUIRED and rejects the call before the handler runs.
func TestLaborReportTool_OptionalFiltersMayBeOmitted(t *testing.T) {
	deps := newEvalDeps()
	reports := &fakeReportsClient{}
	deps.Reports = reports
	sess := wireSession(t, deps)

	res, err := sess.CallTool(t.Context(), &sdk.CallToolParams{
		Name:      "get_workforce_labor_report",
		Arguments: map[string]any{"from": "2026-06-01T00:00:00Z", "to": "2026-06-02T00:00:00Z"},
	})
	if err != nil {
		t.Fatalf("call without the optional filters: %v", err)
	}
	if res.IsError {
		t.Fatalf("call without the optional filters was rejected: %+v", res.Content)
	}
	if reports.lastQuery.From != "2026-06-01T00:00:00Z" || reports.lastQuery.PathId != "" || reports.lastQuery.Granularity != "" {
		t.Fatalf("the reports client got %+v, want the window and no filters", reports.lastQuery)
	}
}
