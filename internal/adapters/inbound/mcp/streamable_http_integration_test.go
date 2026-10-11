//go:build integration

// Integration tests for the MCP inbound adapter over the REAL Streamable
// HTTP transport: mcp.Handler(server) mounted on an httptest.Server, driven
// by the SDK's own client (mcp.NewClient + StreamableClientTransport), with
// the REAL Postgres-backed use cases behind it — exactly the deployment
// shape cmd/mcp wires (ADR-0008: Streamable HTTP only). This proves the
// wire contract (initialize, tools/list, tools/call) end-to-end against
// real persistence, not the tool handlers in isolation.
//
// Postgres comes from testcontainers (TestMain in this file): one container
// per package run, migrated once into a template database, one private
// database per test. Never an external DATABASE_URL, never t.Skip.
package mcp_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	mcpadapter "github.com/claudioed/workforce-management/internal/adapters/inbound/mcp"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// One Postgres container serves the whole package, migrated once into a
// template database; each test gets a private clone (milliseconds). See
// internal/application/usecases/usecases_wiring_integration_test.go for the
// same pattern's rationale. Never an external DATABASE_URL, never t.Skip.
const templateDB = "mcp_migrated_template"

var (
	sharedBaseURL string
	dbSeq         atomic.Uint64
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("workforce_mcp"),
		tcpostgres.WithUsername("workforce"),
		tcpostgres.WithPassword("workforce"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		return 1
	}
	defer func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			fmt.Fprintf(os.Stderr, "terminate postgres container: %v\n", err)
		}
	}()

	var err2 error
	sharedBaseURL, err2 = container.ConnectionString(ctx, "sslmode=disable")
	if err2 != nil {
		fmt.Fprintf(os.Stderr, "postgres connection string: %v\n", err2)
		return 1
	}
	if err := createDatabase(ctx, templateDB); err != nil {
		fmt.Fprintf(os.Stderr, "create template database: %v\n", err)
		return 1
	}
	tmplDB := withDB(sharedBaseURL, templateDB)
	migrations, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "migrations"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve migrations dir: %v\n", err)
		return 1
	}
	if err := postgres.Migrate(tmplDB, migrations); err != nil {
		fmt.Fprintf(os.Stderr, "migrate template: %v\n", err)
		return 1
	}
	return m.Run()
}

// withDB rewrites the path of a connection URL to the named database.
func withDB(baseURL, name string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}

// createDatabase creates an empty database inside the shared container.
func createDatabase(ctx context.Context, name string) error {
	conn, err := pgx.Connect(ctx, sharedBaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

// migratedDB hands the test a connection URL to its own private database
// cloned from the migrated template.
func migratedDB(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("mcp_%d", dbSeq.Add(1))
	conn, err := pgx.Connect(context.Background(), sharedBaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), fmt.Sprintf(
		"CREATE DATABASE %q WITH TEMPLATE %q", name, templateDB)); err != nil {
		t.Fatalf("clone database: %v", err)
	}
	return withDB(sharedBaseURL, name)
}

// itFixedClock is the ports.Clock the use cases accept; deterministic
// timestamps keep the seeded state comparable. (The package's other tests
// have their own fixedClock — this one belongs to the integration suite.)
type itFixedClock struct{ now time.Time }

func (c itFixedClock) Now() time.Time { return c.now }

// itFixedCapacity scripts installed capacity per station CAPABILITY, the
// key fulfillment-execution actually counts by (a stand-in for the live
// registry the composition root fetches over HTTP; the fleet's other
// integration suites script it the same way).
type itFixedCapacity map[shared.Capability]int

func (c itFixedCapacity) InstalledCapacity(_ context.Context, capability shared.Capability) (int, error) {
	return c[capability], nil
}

// itCatalogue mirrors the fleet's real process-path catalogue
// (process-path-management / warehouse-infra sortable-fc.yaml), the same
// shape cmd/mcp wires: UPPER-case canonical path ids, each requiring the
// lower-case capability stations are registered with in
// fulfillment-execution.
func itCatalogue() *pathcatalog.Catalogue {
	return pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PICK", MatchPrefix: "pick", RequiredCapabilities: []string{"pick"}},
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
		{Id: "REBIN", MatchPrefix: "rebin", RequiredCapabilities: []string{"rebin"}},
		{Id: "SLAM", MatchPrefix: "slam", RequiredCapabilities: []string{"slam"}},
	})
}

// mcpHarness wires the REAL production stack — Postgres repos, UnitOfWork,
// use cases, mcp.NewServer, mcp.Handler — and serves it over HTTP. It
// returns a connected SDK client session; the test drives tools/list and
// tools/call exactly like a model host would.
type mcpHarness struct {
	session   *sdkmcp.ClientSession
	publisher *events.LogPublisher
	assign    *usecases.AssignLabor
}

// newMCPHarness seeds one committed PACK plan and one pack-certified
// associate on a fresh private database, then serves the real MCP stack
// over Streamable HTTP.
func newMCPHarness(t *testing.T) *mcpHarness {
	t.Helper()
	ctx := context.Background()
	pool, err := postgres.NewPool(ctx, migratedDB(t))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	publisher := events.NewLogPublisher(nil)
	clock := itFixedClock{time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	uow := postgres.NewUnitOfWork(pool)
	associates := postgres.NewAssociateRepo(pool)
	shiftPlans := postgres.NewShiftPlanRepo(pool)
	assignments := postgres.NewAssignmentRepo(pool)

	// Seed: commit a two-head pack plan so get_staffing_gap has a real
	// committed plan to read, and start one pack-certified associate at
	// the site so assign_labor has someone real to place.
	commit := &usecases.CommitShiftPlan{
		ShiftPlans: shiftPlans, Events: publisher, Clock: clock,
		InstalledCapacity: itFixedCapacity{"pack": 10}, Catalogue: itCatalogue(),
		MaxHoursPerShift: 8, UnitOfWork: uow,
	}
	if _, err := commit.Execute(ctx, "WH1", "S1",
		[]shiftplan.PathPlan{{PathId: "PACK", PlannedHeads: 2, PlannedRate: 50, PlannedHours: 8}},
		map[shared.PathId]int{"PACK": 10},
	); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	start := &usecases.StartAssociateShift{
		Associates: associates, Events: publisher, Clock: clock, UnitOfWork: uow,
	}
	if _, err := start.ExecuteAtSite(ctx, "ITCOV-MCP-1", []shared.Certification{"pack"}, "WH1"); err != nil {
		t.Fatalf("seed associate: %v", err)
	}

	// The exact Deps shape cmd/mcp wires (minus the optional reports
	// client, which registers an extra read tool when present).
	server := mcpadapter.NewServer(mcpadapter.Deps{
		GetStaffingGap: &usecases.GetStaffingGap{
			ShiftPlans: shiftPlans, Assignments: assignments,
			Events: publisher, Clock: clock, UnitOfWork: uow,
		},
		ProposePathPlan: &usecases.ProposePathPlan{
			Events: publisher, Clock: clock, UnitOfWork: uow,
		},
		AssignLabor: &usecases.AssignLabor{
			Associates: associates, Assignments: assignments,
			Events: publisher, Clock: clock, MaxHoursPerShift: 8,
			Catalogue: itCatalogue(), UnitOfWork: uow,
		},
		Catalogue: itCatalogue(),
	})

	hs := httptest.NewServer(mcpadapter.Handler(server))
	t.Cleanup(hs.Close)

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "itcov-test-host", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint: hs.URL, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return &mcpHarness{
		session:   session,
		publisher: publisher,
		assign: &usecases.AssignLabor{
			Associates: associates, Assignments: assignments,
			Events: publisher, Clock: clock, MaxHoursPerShift: 8,
			Catalogue: itCatalogue(), UnitOfWork: uow,
		},
	}
}

func TestMCP_ListToolsExposesTheContract(t *testing.T) {
	h := newMCPHarness(t)
	ctx := context.Background()

	list, err := h.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"get_staffing_gap", "propose_path_heads", "assign_labor"} {
		if !names[want] {
			t.Fatalf("tools/list must expose %q, got %v", want, names)
		}
	}
	// The write tool must be annotated non-read-only so a host can gate it.
	for _, tool := range list.Tools {
		if tool.Name == "assign_labor" {
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint {
				t.Fatal("assign_labor must carry ReadOnlyHint=false")
			}
		}
	}
}

func TestMCP_CallToolRoundTripThroughPostgres(t *testing.T) {
	h := newMCPHarness(t)
	ctx := context.Background()

	// Read tool: the staffing gap over the real seeded plan (2 planned,
	// 0 active => understaffed).
	gap, err := h.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "get_staffing_gap",
		Arguments: map[string]any{
			"siteCode": "WH1", "shiftId": "S1", "pathId": "PACK",
		},
	})
	if err != nil {
		t.Fatalf("tools/call get_staffing_gap: %v", err)
	}
	if gap.IsError {
		t.Fatalf("get_staffing_gap returned a tool error: %+v", gap)
	}
	sc, ok := gap.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("get_staffing_gap must return structured content, got %+v", gap.Content)
	}
	if sc["plannedHeads"].(float64) != 2 || sc["understaffed"].(bool) != true {
		t.Fatalf("expected plannedHeads=2 understaffed=true over the seeded plan, got %v", sc)
	}

	// Pure-computation read tool: heads = ceil(charge/rate).
	heads, err := h.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "propose_path_heads",
		Arguments: map[string]any{
			"siteCode": "WH1", "pathId": "PACK", "charge": 100, "plannedRate": 30,
		},
	})
	if err != nil {
		t.Fatalf("tools/call propose_path_heads: %v", err)
	}
	if heads.IsError {
		t.Fatalf("propose_path_heads returned a tool error: %+v", heads)
	}
	if ph, ok := heads.StructuredContent.(map[string]any); !ok || ph["proposedHeads"].(float64) != 4 {
		t.Fatalf("expected proposedHeads=4 (ceil(100/30)), got %+v", heads.StructuredContent)
	}

	// Write tool: assign the seeded associate through the real use case
	// stack; the assignment must really be persisted and published.
	assignRes, err := h.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "assign_labor",
		Arguments: map[string]any{"associateId": "ITCOV-MCP-1", "pathId": "PACK"},
	})
	if err != nil {
		t.Fatalf("tools/call assign_labor: %v", err)
	}
	if assignRes.IsError {
		t.Fatalf("assign_labor returned a tool error: %+v", assignRes)
	}
	la, err := h.assign.Execute(ctx, "NOBODY", "PACK")
	if err == nil || la != nil {
		t.Fatal("sanity: the wired use case must reject an unknown associate")
	}
	var sawAssigned bool
	for _, e := range h.publisher.Events() {
		if e.EventName() == "LaborAssigned" {
			sawAssigned = true
		}
	}
	if !sawAssigned {
		t.Fatal("assign_labor must publish LaborAssigned through the real stack")
	}

	// Domain rejection as a TOOL error, never a transport error: the
	// associate is unknown to Postgres.
	unknown, err := h.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "assign_labor",
		Arguments: map[string]any{"associateId": "ITCOV-GHOST", "pathId": "PACK"},
	})
	if err != nil {
		t.Fatalf("tools/call assign_labor (unknown associate): %v", err)
	}
	if !unknown.IsError {
		t.Fatal("assigning an unknown associate must surface a tool error, not success")
	}
}

func TestMCP_CallToolRejectsInvalidInput(t *testing.T) {
	h := newMCPHarness(t)
	ctx := context.Background()

	// Missing shiftId: the adapter must answer a tool error carrying the
	// validation failure, never a transport-level failure.
	res, err := h.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "get_staffing_gap",
		Arguments: map[string]any{"siteCode": "WH1", "shiftId": "", "pathId": "PACK"},
	})
	if err != nil {
		t.Fatalf("tools/call with invalid input: %v", err)
	}
	if !res.IsError {
		t.Fatal("empty shiftId must surface a tool error")
	}

	// A pathId outside the fleet catalogue (ADR-0013) is rejected before
	// it reaches a use case — again as a tool error.
	outside, err := h.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "propose_path_heads",
		Arguments: map[string]any{"siteCode": "WH1", "pathId": "nonexistent", "charge": 10, "plannedRate": 5},
	})
	if err != nil {
		t.Fatalf("tools/call with unknown pathId: %v", err)
	}
	if !outside.IsError {
		t.Fatal("a pathId outside the catalogue must surface a tool error")
	}

	// plannedRate <= 0 is a validation failure, not a domain computation.
	badRate, err := h.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "propose_path_heads",
		Arguments: map[string]any{"siteCode": "WH1", "pathId": "PACK", "charge": 10, "plannedRate": 0},
	})
	if err != nil {
		t.Fatalf("tools/call with plannedRate=0: %v", err)
	}
	if !badRate.IsError {
		t.Fatal("plannedRate=0 must surface a tool error")
	}
}
