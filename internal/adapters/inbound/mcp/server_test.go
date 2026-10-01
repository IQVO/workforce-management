package mcp_test

import (
	"context"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	inboundmcp "github.com/claudioed/workforce-management/internal/adapters/inbound/mcp"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// fixedClock is a ports.Clock returning a fixed instant for deterministic
// use-case timing over the wire.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// unlimitedInstalledCapacity is a test double for
// ports.InstalledCapacityClient that always reports capacity as
// effectively unlimited, so tests focused on other behavior don't need
// to separately script the live-capacity fetch Feature C introduced.
type unlimitedInstalledCapacity struct{}

func (unlimitedInstalledCapacity) InstalledCapacity(_ context.Context, _ shared.Capability) (int, error) {
	return math.MaxInt32, nil
}

// newServer builds a real MCP HTTP server over in-memory repos seeded with a
// committed pack plan (plannedHeads=3) and one pack-certified associate a1.
// Returns the httptest URL.
func newServer(t *testing.T) string {
	t.Helper()
	associates := memory.NewAssociateRepo()
	shiftPlans := memory.NewShiftPlanRepo()
	assignments := memory.NewAssignmentRepo()
	publisher := events.NewLogPublisher(nil)
	clk := fixedClock{now: base}
	const maxHours = 10.0
	ctx := context.Background()

	commit := &usecases.CommitShiftPlan{ShiftPlans: shiftPlans, Events: publisher, Clock: clk, InstalledCapacity: unlimitedInstalledCapacity{}, Catalogue: fleetCatalogue(), MaxHoursPerShift: maxHours}
	if _, err := commit.Execute(ctx, "B1", "S1",
		[]shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 3, PlannedRate: 10, PlannedHours: 0}},
		map[shared.PathId]int{"pack": 20},
	); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	start := &usecases.StartAssociateShift{Associates: associates, Events: publisher, Clock: clk}
	if _, err := start.Execute(ctx, "a1", []shared.Certification{"pack"}); err != nil {
		t.Fatalf("seed associate: %v", err)
	}

	deps := inboundmcp.Deps{
		GetStaffingGap:  &usecases.GetStaffingGap{ShiftPlans: shiftPlans, Assignments: assignments, Events: publisher, Clock: clk},
		ProposePathPlan: &usecases.ProposePathPlan{Events: publisher, Clock: clk},
		AssignLabor:     &usecases.AssignLabor{Associates: associates, Assignments: assignments, Events: publisher, Clock: clk, MaxHoursPerShift: maxHours},
	}
	server := inboundmcp.NewServer(deps)
	httpSrv := httptest.NewServer(inboundmcp.Handler(server))
	t.Cleanup(httpSrv.Close)
	return httpSrv.URL
}

func connect(t *testing.T, url string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	transport := &sdk.StreamableClientTransport{Endpoint: url}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestServer_ToolsListAndCall(t *testing.T) {
	url := newServer(t)
	session := connect(t, url)
	ctx := context.Background()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := map[string]bool{"get_staffing_gap": false, "propose_path_heads": false, "assign_labor": false}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q not advertised", name)
		}
	}

	res, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "get_staffing_gap",
		Arguments: map[string]any{"buildingId": "B1", "shiftId": "S1", "pathId": "pack"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %+v", res.Content)
	}
	sc := res.StructuredContent.(map[string]any)
	if sc["plannedHeads"].(float64) != 3 {
		t.Fatalf("plannedHeads = %v, want 3", sc["plannedHeads"])
	}
	if sc["understaffed"].(bool) != true {
		t.Fatalf("understaffed = %v, want true", sc["understaffed"])
	}
}

func TestServer_CallToolRejectsMissingArgs(t *testing.T) {
	url := newServer(t)
	session := connect(t, url)
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_staffing_gap",
		Arguments: map[string]any{"buildingId": "B1", "shiftId": "S1", "pathId": ""},
	})
	if err != nil {
		t.Fatalf("call tool transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool-level error for empty pathId")
	}
}

func TestServer_ResourceRead(t *testing.T) {
	url := newServer(t)
	session := connect(t, url)
	res, err := session.ReadResource(context.Background(), &sdk.ReadResourceParams{
		URI: "staffing://B1/S1/pack/gap",
	})
	if err != nil {
		t.Fatalf("read resource: %v", err)
	}
	if len(res.Contents) == 0 || res.Contents[0].Text == "" {
		t.Fatalf("empty resource contents: %+v", res.Contents)
	}
}

func TestServer_PromptGet(t *testing.T) {
	url := newServer(t)
	session := connect(t, url)
	res, err := session.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: "cover_staffing_gaps"})
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatal("cover_staffing_gaps prompt returned no messages")
	}
}

func TestServer_AssignLaborSucceeds(t *testing.T) {
	url := newServer(t)
	session := connect(t, url)
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "assign_labor",
		Arguments: map[string]any{"associateId": "a1", "pathId": "pack"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("assign_labor returned error: %+v", res.Content)
	}
	sc := res.StructuredContent.(map[string]any)
	if sc["associateId"] != "a1" || sc["pathId"] != "pack" {
		t.Fatalf("unexpected structured content: %+v", sc)
	}

	// A certification-mismatch assignment is rejected over the wire: a1 is not
	// certified for pick.
	bad, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "assign_labor",
		Arguments: map[string]any{"associateId": "a1", "pathId": "pick"},
	})
	if err != nil {
		t.Fatalf("second call transport error: %v", err)
	}
	if !bad.IsError {
		t.Fatal("assign_labor to an uncertified path must be rejected by the certification invariant")
	}
}

// fleetCatalogue mirrors the fleet's real process-path catalogue
// (process-path-management / warehouse-infra sortable-fc.yaml):
// UPPER-case canonical path ids, each requiring the lower-case capability
// stations are registered with in fulfillment-execution. CommitShiftPlan
// resolves a line's path through it to the capability it queries.
func fleetCatalogue() *pathcatalog.Catalogue {
	return pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PICK", MatchPrefix: "pick", RequiredCapabilities: []string{"pick"}},
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
		{Id: "REBIN", MatchPrefix: "rebin", RequiredCapabilities: []string{"rebin"}},
		{Id: "SLAM", MatchPrefix: "slam", RequiredCapabilities: []string{"slam"}},
	})
}
