package a2a

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type unlimitedInstalledCapacity struct{}

func (unlimitedInstalledCapacity) InstalledCapacity(_ context.Context, _ shared.Capability) (int, error) {
	return math.MaxInt32, nil
}

func fleetCatalogue() *pathcatalog.Catalogue {
	return pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PICK", MatchPrefix: "pick", RequiredCapabilities: []string{"pick"}},
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
	})
}

// harness builds Deps over in-memory repos, seeding through the real use
// cases with a fixed clock — the same approach as the MCP adapter's harness.
type harness struct {
	deps   Deps
	start  *usecases.StartAssociateShift
	commit *usecases.CommitShiftPlan
}

func newHarness(t *testing.T, withCatalogue bool) *harness {
	t.Helper()
	associates := memory.NewAssociateRepo()
	shiftPlans := memory.NewShiftPlanRepo()
	assignments := memory.NewAssignmentRepo().WithAssociates(associates)
	publisher := events.NewLogPublisher(nil)
	clk := fixedClock{now: base}
	const maxHours = 10.0

	h := &harness{
		deps: Deps{
			GetStaffingGap:  &usecases.GetStaffingGap{ShiftPlans: shiftPlans, Assignments: assignments, Events: publisher, Clock: clk},
			ProposePathPlan: &usecases.ProposePathPlan{Events: publisher, Clock: clk},
			AssignLabor:     &usecases.AssignLabor{Associates: associates, Assignments: assignments, Events: publisher, Clock: clk, MaxHoursPerShift: maxHours},
		},
		start:  &usecases.StartAssociateShift{Associates: associates, Events: publisher, Clock: clk},
		commit: &usecases.CommitShiftPlan{ShiftPlans: shiftPlans, Events: publisher, Clock: clk, InstalledCapacity: unlimitedInstalledCapacity{}, Catalogue: fleetCatalogue(), MaxHoursPerShift: maxHours},
	}
	if withCatalogue {
		h.deps.Catalogue = fleetCatalogue()
	}
	return h
}

func (h *harness) seedPlan(t *testing.T, siteCode, shiftId, pathId string, plannedHeads int) {
	t.Helper()
	lines := []shiftplan.PathPlan{{PathId: shared.PathId(pathId), PlannedHeads: plannedHeads, PlannedRate: 10}}
	installed := map[shared.PathId]int{shared.PathId(pathId): plannedHeads + 10}
	if _, err := h.commit.Execute(context.Background(), siteCode, shiftId, lines, installed); err != nil {
		t.Fatalf("seedPlan: %v", err)
	}
}

// seedAssociate starts an associate AT WH1. The A2A surface takes the
// canonical siteCode, which scopes the staffing gap to associates at that site
// (ADR 0034); an associate with no site would not be counted.
func (h *harness) seedAssociate(t *testing.T, id string, certs ...string) {
	t.Helper()
	cc := make([]shared.Certification, 0, len(certs))
	for _, c := range certs {
		cc = append(cc, shared.Certification(c))
	}
	if _, err := h.start.ExecuteAtSite(context.Background(), shared.AssociateId(id), cc, "WH1"); err != nil {
		t.Fatalf("seedAssociate: %v", err)
	}
}

// ---- the wire: JSON-RPC over HTTP, driven by the SDK's own client ----------

// wireEnvelope is a TEST-LOCAL copy of the answer contract. If the adapter's
// envelope drifts, this suite fails instead of silently following it.
type wireEnvelope struct {
	Skill string `json:"skill"`
	Data  struct {
		StaffingStatus *struct {
			Gap struct {
				SiteCode     string `json:"siteCode"`
				ShiftId      string `json:"shiftId"`
				PathId       string `json:"pathId"`
				PlannedHeads int    `json:"plannedHeads"`
				ActiveHeads  int    `json:"activeHeads"`
				Understaffed bool   `json:"understaffed"`
			} `json:"gap"`
			HeadsSizing *struct {
				Charge        float64 `json:"charge"`
				PlannedRate   float64 `json:"plannedRate"`
				ProposedHeads int     `json:"proposedHeads"`
			} `json:"headsSizing"`
		} `json:"staffingStatus"`
		LaborAssigned *struct {
			AssociateId  string `json:"associateId"`
			PathId       string `json:"pathId"`
			AssignmentId string `json:"assignmentId"`
		} `json:"laborAssigned"`
	} `json:"data"`
	Problem *struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Status int    `json:"status"`
	} `json:"problem"`
}

type wire struct {
	client *a2aclient.Client
}

func newWire(t *testing.T, deps Deps) *wire {
	t.Helper()
	srv := httptest.NewServer(Handler(NewExecutor(deps)))
	t.Cleanup(srv.Close)
	card := Card(Config{AgentURL: srv.URL + "/", Version: "test"})
	client, err := a2aclient.NewFromCard(context.Background(), card)
	if err != nil {
		t.Fatalf("NewFromCard: %v", err)
	}
	return &wire{client: client}
}

// call sends one message part and returns the decoded envelope and the
// one-line text summary of the agent's reply.
func (w *wire) call(t *testing.T, part *a2a.Part) (wireEnvelope, string) {
	t.Helper()
	resp, err := w.client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, part)})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	msg, ok := resp.(*a2a.Message)
	if !ok || len(msg.Parts) < 2 {
		t.Fatalf("reply = %T, want a message with a data and a text part", resp)
	}
	data, ok := msg.Parts[0].Content.(a2a.Data)
	if !ok {
		t.Fatalf("first reply part is %T, want data", msg.Parts[0].Content)
	}
	raw, _ := json.Marshal(data.Value)
	var env wireEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("reply is not the skill envelope: %v (%s)", err, raw)
	}
	text, _ := msg.Parts[1].Content.(a2a.Text)
	return env, string(text)
}

func (w *wire) skill(t *testing.T, skill string, args map[string]any) (wireEnvelope, string) {
	t.Helper()
	return w.call(t, a2a.NewDataPart(map[string]any{"skill": skill, "arguments": args}))
}

func wantProblem(t *testing.T, env wireEnvelope, status int, slug string) {
	t.Helper()
	if env.Problem == nil {
		t.Fatalf("expected a %d problem, got data: %+v", status, env.Data)
	}
	if env.Problem.Status != status || env.Problem.Type != problemTypeBase+"/"+slug {
		t.Fatalf("problem = %+v, want %d %s", env.Problem, status, slug)
	}
}

func TestWireStaffingStatusReadsTheGapAndSizesOnRequest(t *testing.T) {
	h := newHarness(t, false)
	h.seedPlan(t, "WH1", "S1", "pack", 3)
	h.seedAssociate(t, "a1", "pack")
	if _, err := h.deps.AssignLabor.Execute(context.Background(), "a1", "pack"); err != nil {
		t.Fatal(err)
	}
	w := newWire(t, h.deps)

	env, text := w.skill(t, skillStaffingStatus, map[string]any{"siteCode": "WH1", "shiftId": "S1", "pathId": "pack"})
	g := env.Data.StaffingStatus
	if env.Problem != nil || g == nil {
		t.Fatalf("envelope = %+v", env)
	}
	if g.Gap.PlannedHeads != 3 || g.Gap.ActiveHeads != 1 || !g.Gap.Understaffed || g.Gap.SiteCode != "WH1" || g.HeadsSizing != nil {
		t.Fatalf("gap = %+v sizing = %+v, want 1 of 3 understaffed and no sizing unless asked", g.Gap, g.HeadsSizing)
	}
	if text != "WH1/S1/pack: 1 of 3 planned heads active, UNDERSTAFFED" {
		t.Fatalf("summary = %q", text)
	}

	env, text = w.skill(t, skillStaffingStatus, map[string]any{"siteCode": "WH1", "shiftId": "S1", "pathId": "pack", "charge": 120, "plannedRate": 30})
	g = env.Data.StaffingStatus
	if g == nil || g.HeadsSizing == nil || g.HeadsSizing.ProposedHeads != 4 {
		t.Fatalf("sizing = %+v, want ceil(120/30)=4 heads", g)
	}
	if text != "WH1/S1/pack: 1 of 3 planned heads active, UNDERSTAFFED; sizing: 4 heads for charge 120 at rate 30" {
		t.Fatalf("summary = %q", text)
	}
}

func TestWireAssignLaborMovesTheGapAndIsAnswered(t *testing.T) {
	h := newHarness(t, false)
	h.seedPlan(t, "WH1", "S1", "pack", 2)
	h.seedAssociate(t, "a1", "pack")
	w := newWire(t, h.deps)

	env, text := w.skill(t, skillAssignLabor, map[string]any{"associateId": "a1", "pathId": "pack"})
	la := env.Data.LaborAssigned
	if env.Problem != nil || la == nil || la.AssociateId != "a1" || la.PathId != "pack" || la.AssignmentId != "a1@pack" {
		t.Fatalf("envelope = %+v", env)
	}
	if text != "assigned a1 to pack" {
		t.Fatalf("summary = %q", text)
	}

	env, _ = w.skill(t, skillStaffingStatus, map[string]any{"siteCode": "WH1", "shiftId": "S1", "pathId": "pack"})
	if g := env.Data.StaffingStatus; g == nil || g.Gap.ActiveHeads != 1 {
		t.Fatalf("after assigning, active heads = %+v, want 1: the delegated write really happened", g)
	}
}

func TestWireDomainRejectionsAreAnsweredNotRaised(t *testing.T) {
	h := newHarness(t, false)
	h.seedPlan(t, "WH1", "S1", "pack", 2)
	h.seedAssociate(t, "uncertified", "pick")
	w := newWire(t, h.deps)

	env, _ := w.skill(t, skillAssignLabor, map[string]any{"associateId": "uncertified", "pathId": "pack"})
	wantProblem(t, env, 409, "business-rule")

	env, _ = w.skill(t, skillAssignLabor, map[string]any{"associateId": "ghost", "pathId": "pack"})
	wantProblem(t, env, 404, "not-found")

	env, _ = w.skill(t, skillStaffingStatus, map[string]any{"siteCode": "NOPE", "shiftId": "S9", "pathId": "pack"})
	wantProblem(t, env, 404, "not-found")
}

func TestWireArgumentValidation(t *testing.T) {
	w := newWire(t, newHarness(t, false).deps)
	staffing := func(args map[string]any) map[string]any { return args }
	cases := map[string]struct {
		skill string
		args  map[string]any
		slug  string
	}{
		"missing siteCode":          {skillStaffingStatus, staffing(map[string]any{"shiftId": "S1", "pathId": "pack"}), "validation"},
		"missing shiftId":           {skillStaffingStatus, staffing(map[string]any{"siteCode": "WH1", "pathId": "pack"}), "invalid-argument"},
		"missing pathId":            {skillStaffingStatus, staffing(map[string]any{"siteCode": "WH1", "shiftId": "S1"}), "invalid-argument"},
		"charge without rate":       {skillStaffingStatus, staffing(map[string]any{"siteCode": "WH1", "shiftId": "S1", "pathId": "pack", "charge": 10}), "invalid-argument"},
		"rate without charge":       {skillStaffingStatus, staffing(map[string]any{"siteCode": "WH1", "shiftId": "S1", "pathId": "pack", "plannedRate": 10}), "invalid-argument"},
		"negative charge":           {skillStaffingStatus, staffing(map[string]any{"siteCode": "WH1", "shiftId": "S1", "pathId": "pack", "charge": -1, "plannedRate": 10}), "invalid-argument"},
		"zero rate":                 {skillStaffingStatus, staffing(map[string]any{"siteCode": "WH1", "shiftId": "S1", "pathId": "pack", "charge": 5, "plannedRate": 0}), "invalid-argument"},
		"deprecated alias refused":  {skillStaffingStatus, staffing(map[string]any{"buildingId": "WH1", "shiftId": "S1", "pathId": "pack"}), "invalid-argument"},
		"typo'd optional argument":  {skillStaffingStatus, staffing(map[string]any{"siteCode": "WH1", "shiftId": "S1", "pathId": "pack", "charge": 5, "plannedRte": 10}), "invalid-argument"},
		"assign: missing associate": {skillAssignLabor, map[string]any{"pathId": "pack"}, "invalid-argument"},
		"assign: missing path":      {skillAssignLabor, map[string]any{"associateId": "a1"}, "invalid-argument"},
		"assign: unknown argument":  {skillAssignLabor, map[string]any{"associateId": "a1", "pathId": "pack", "force": true}, "invalid-argument"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env, _ := w.skill(t, tc.skill, tc.args)
			wantProblem(t, env, 400, tc.slug)
		})
	}
}

// TestWireNeverTeachesTheDeprecatedAlias: the shared domain error for a missing
// site key mentions buildingId, which this surface does not offer. No answer an
// agent can receive may mention it.
func TestWireNeverTeachesTheDeprecatedAlias(t *testing.T) {
	w := newWire(t, newHarness(t, false).deps)
	env, text := w.skill(t, skillStaffingStatus, map[string]any{"shiftId": "S1", "pathId": "pack"})
	wantProblem(t, env, 400, "validation")
	if env.Problem.Detail != "siteCode is required" {
		t.Fatalf("detail = %q, want exactly %q", env.Problem.Detail, "siteCode is required")
	}
	for _, s := range []string{env.Problem.Detail, env.Problem.Title, text} {
		if strings.Contains(s, "buildingId") {
			t.Fatalf("an answer teaches the deprecated alias: %q", s)
		}
	}
}

func TestWireValidatesPathIdsAgainstTheCatalogue(t *testing.T) {
	w := newWire(t, newHarness(t, true).deps)
	env, _ := w.skill(t, skillStaffingStatus, map[string]any{"siteCode": "WH1", "shiftId": "S1", "pathId": "ghost"})
	wantProblem(t, env, 400, "validation")
	env, _ = w.skill(t, skillAssignLabor, map[string]any{"associateId": "a1", "pathId": "ghost"})
	wantProblem(t, env, 400, "validation")
}

func TestWireRequestShapeProblems(t *testing.T) {
	w := newWire(t, newHarness(t, false).deps)

	env, _ := w.call(t, a2a.NewTextPart("not json at all"))
	wantProblem(t, env, 400, "malformed-skill-request")

	env, _ = w.call(t, a2a.NewDataPart(map[string]any{"arguments": map[string]any{}}))
	wantProblem(t, env, 400, "malformed-skill-request")

	env, _ = w.call(t, a2a.NewDataPart(map[string]any{"skill": "staffing_status", "arguments": map[string]any{}, "extra": 1}))
	wantProblem(t, env, 400, "malformed-skill-request")

	env, _ = w.skill(t, "launch_rocket", map[string]any{})
	wantProblem(t, env, 400, "unknown-skill")

	// A text part holding the same JSON is accepted (documented contract).
	h := newHarness(t, false)
	h.seedPlan(t, "WH1", "S1", "pack", 1)
	tw := newWire(t, h.deps)
	env, _ = tw.call(t, a2a.NewTextPart(`{"skill":"staffing_status","arguments":{"siteCode":"WH1","shiftId":"S1","pathId":"pack"}}`))
	if env.Problem != nil || env.Data.StaffingStatus == nil {
		t.Fatalf("a text part holding the skill JSON must be accepted, got %+v", env)
	}
}

// TestCardIsServedAndDialable proves discovery end to end: the card handler
// serves the card at the well-known path and a client built from the FETCHED
// card reaches the agent.
func TestCardIsServedAndDialable(t *testing.T) {
	h := newHarness(t, false)
	h.seedPlan(t, "WH1", "S1", "pack", 1)
	mux := http.NewServeMux()
	mux.Handle("/", Handler(NewExecutor(h.deps)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(Card(Config{AgentURL: srv.URL + "/", Version: "test"})))

	res, err := http.Get(srv.URL + a2asrv.WellKnownAgentCardPath)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("card fetch = %v / %v", res, err)
	}
	defer func() { _ = res.Body.Close() }()
	var card a2a.AgentCard
	if err := json.NewDecoder(res.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	client, err := a2aclient.NewFromCard(context.Background(), &card)
	if err != nil {
		t.Fatal(err)
	}
	w := &wire{client: client}
	env, _ := w.skill(t, skillStaffingStatus, map[string]any{"siteCode": "WH1", "shiftId": "S1", "pathId": "pack"})
	if env.Data.StaffingStatus == nil {
		t.Fatalf("a client built from the served card could not call the agent: %+v", env)
	}
}
