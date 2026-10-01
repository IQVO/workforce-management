// Shared harness for the MCP eval suites (E1-E3): one place that builds a
// real Streamable HTTP server over in-memory adapters and connects a real
// SDK client to it, so schema evals, wire conformance evals, and the
// Gherkin behavioral evals all exercise exactly the surface a model host
// would. The mcp package tests need no Postgres: Deps are wired over the
// in-memory repos, seeded through the real use cases.
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
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// evalClockBase is the fixed instant every eval seeds and asserts around.
var evalClockBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// maxEvalHours is the harness's MaxHoursPerShift, matching server_test.go.
const maxEvalHours = 10.0

// evalFixedClock is a ports.Clock returning a fixed instant (server_test.go
// keeps its own type in this package; this one belongs to the eval harness).
type evalFixedClock struct{ now time.Time }

func (c evalFixedClock) Now() time.Time { return c.now }

// evalUnlimitedCapacity is a ports.InstalledCapacityClient double that
// never constrains plan commits, so eval seeding never scripts capacity.
type evalUnlimitedCapacity struct{}

func (evalUnlimitedCapacity) InstalledCapacity(_ context.Context, _ shared.Capability) (int, error) {
	return math.MaxInt32, nil
}

// evalHarness is a fully wired MCP server over in-memory repos plus the
// client session talking to it, with the knobs the evals assert on.
type evalHarness struct {
	session         *sdk.ClientSession
	server          *httptest.Server
	associates      *memory.AssociateRepo
	shiftPlans      *memory.ShiftPlanRepo
	assignments     *memory.AssignmentRepo
	publisher       *events.LogPublisher
	clock           evalFixedClock
	reports         *fakeReportsClient
	lastCallResult  *sdk.CallToolResult
	lastCallErr     error
	lastCallContent string
}

// newEvalDeps builds the DEFAULT tool surface (no reports client, so the
// curated report tool is not registered) over empty in-memory repos — the
// shape the goldens pin. Enough for schema and conformance evals that do
// not seed state.
func newEvalDeps() inboundmcp.Deps {
	associates := memory.NewAssociateRepo()
	shiftPlans := memory.NewShiftPlanRepo()
	assignments := memory.NewAssignmentRepo()
	publisher := events.NewLogPublisher(nil)
	clk := evalFixedClock{now: evalClockBase}
	return inboundmcp.Deps{
		GetStaffingGap:  &usecases.GetStaffingGap{ShiftPlans: shiftPlans, Assignments: assignments, Events: publisher, Clock: clk},
		ProposePathPlan: &usecases.ProposePathPlan{Events: publisher, Clock: clk},
		AssignLabor:     &usecases.AssignLabor{Associates: associates, Assignments: assignments, Events: publisher, Clock: clk, MaxHoursPerShift: maxEvalHours},
	}
}

// newEvalHarness seeds the canonical eval state over a real Streamable HTTP
// server and connects a client session to it. Canonical state:
//
//   - committed plan B1/S1 with pack plannedHeads=3 at rate 10;
//   - associate a1 (pack-certified), a2 (pick-certified),
//     a3 (pack+pick-certified), a4 (pack-certified, ON BREAK);
//   - a reports client serving one deterministic labor-report row, so the
//     conditional get_workforce_labor_report tool is registered and every
//     tool category has a happy path to pin.
func newEvalHarness(t *testing.T) *evalHarness {
	t.Helper()

	h := &evalHarness{}
	h.associates = memory.NewAssociateRepo()
	h.shiftPlans = memory.NewShiftPlanRepo()
	h.assignments = memory.NewAssignmentRepo()
	h.publisher = events.NewLogPublisher(nil)
	h.clock = evalFixedClock{now: evalClockBase}
	h.reports = &fakeReportsClient{report: inboundmcp.LaborReportView{
		Rows: []inboundmcp.LaborRowView{
			{PathId: "pack", HourBucket: "2026-06-01T10:00:00Z", LaborAssigned: 3, LaborReassigned: 1},
		},
	}}
	ctx := context.Background()

	commit := &usecases.CommitShiftPlan{ShiftPlans: h.shiftPlans, Events: h.publisher, Clock: h.clock, InstalledCapacity: evalUnlimitedCapacity{}, Catalogue: fleetCatalogue(), MaxHoursPerShift: maxEvalHours}
	if _, err := commit.Execute(ctx, "B1", "S1",
		[]shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 3, PlannedRate: 10, PlannedHours: 0}},
		map[shared.PathId]int{"pack": 13},
	); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	start := &usecases.StartAssociateShift{Associates: h.associates, Events: h.publisher, Clock: h.clock}
	for _, seed := range []struct {
		id    string
		certs []string
	}{
		{"a1", []string{"pack"}},
		{"a2", []string{"pick"}},
		{"a3", []string{"pack", "pick"}},
		{"a4", []string{"pack"}},
	} {
		cc := make([]shared.Certification, 0, len(seed.certs))
		for _, c := range seed.certs {
			cc = append(cc, shared.Certification(c))
		}
		if _, err := start.Execute(ctx, shared.AssociateId(seed.id), cc); err != nil {
			t.Fatalf("seed associate %s: %v", seed.id, err)
		}
	}
	// a4 goes on break: the break-state conflict scenarios need a
	// pack-certified associate who cannot be assigned right now.
	startBreak := &usecases.StartBreak{Associates: h.associates, Events: h.publisher, Clock: h.clock}
	if err := startBreak.Execute(ctx, "a4"); err != nil {
		t.Fatalf("seed break: %v", err)
	}

	server := inboundmcp.NewServer(inboundmcp.Deps{
		GetStaffingGap:  &usecases.GetStaffingGap{ShiftPlans: h.shiftPlans, Assignments: h.assignments, Events: h.publisher, Clock: h.clock},
		ProposePathPlan: &usecases.ProposePathPlan{Events: h.publisher, Clock: h.clock},
		AssignLabor:     &usecases.AssignLabor{Associates: h.associates, Assignments: h.assignments, Events: h.publisher, Clock: h.clock, MaxHoursPerShift: maxEvalHours},
		Reports:         h.reports,
	})
	h.server = httptest.NewServer(inboundmcp.Handler(server))
	t.Cleanup(h.server.Close)

	client := sdk.NewClient(&sdk.Implementation{Name: "eval-client", Version: "0.0.1"}, nil)
	transport := &sdk.StreamableClientTransport{Endpoint: h.server.URL}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("eval harness connect: %v", err)
	}
	h.session = session
	t.Cleanup(func() { _ = session.Close() })
	return h
}

// wireSession builds a real Streamable HTTP server over the given deps and
// connects a client session to it, for evals that do not need seeded
// state.
func wireSession(t *testing.T, deps inboundmcp.Deps) *sdk.ClientSession {
	t.Helper()
	server := inboundmcp.NewServer(deps)
	httpSrv := httptest.NewServer(inboundmcp.Handler(server))
	t.Cleanup(httpSrv.Close)

	client := sdk.NewClient(&sdk.Implementation{Name: "eval-client", Version: "0.0.1"}, nil)
	transport := &sdk.StreamableClientTransport{Endpoint: httpSrv.URL}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("wire session connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// callTool invokes a tool and records the result for the Then steps.
func (h *evalHarness) callTool(ctx context.Context, name string, args map[string]any) error {
	res, err := h.session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	h.lastCallResult, h.lastCallErr = res, err
	h.lastCallContent = ""
	if res != nil {
		for _, c := range res.Content {
			if text, ok := c.(*sdk.TextContent); ok {
				h.lastCallContent += text.Text
			}
		}
	}
	return err
}
