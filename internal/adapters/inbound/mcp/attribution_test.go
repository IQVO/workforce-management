// Agentic Standard v2 (AS-2): assign_labor over MCP accepts the OPTIONAL
// proposalId / approvedBy / agentId, and the ADAPTER (never the caller) stamps
// via=mcp onto the event the use case publishes.
package mcp_test

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	inboundmcp "github.com/claudioed/workforce-management/internal/adapters/inbound/mcp"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// recordingPublisher captures the events assign_labor publishes.
type recordingPublisher struct{ got []shared.DomainEvent }

func (p *recordingPublisher) Publish(_ context.Context, evts ...shared.DomainEvent) error {
	p.got = append(p.got, evts...)
	return nil
}

// newAttributionSession serves the real MCP handler over in-memory repos with
// one pack/pick-certified associate a1 and returns a client session plus the
// recorder.
func newAttributionSession(t *testing.T) (*sdk.ClientSession, *recordingPublisher) {
	t.Helper()
	associates := memory.NewAssociateRepo()
	assignments := memory.NewAssignmentRepo().WithAssociates(associates)
	rec := &recordingPublisher{}
	clk := evalFixedClock{now: evalClockBase}
	start := &usecases.StartAssociateShift{Associates: associates, Events: rec, Clock: clk}
	if _, err := start.Execute(context.Background(), "a1", []shared.Certification{"pack", "pick"}); err != nil {
		t.Fatalf("seed associate: %v", err)
	}
	rec.got = nil
	deps := inboundmcp.Deps{
		GetStaffingGap:  &usecases.GetStaffingGap{ShiftPlans: memory.NewShiftPlanRepo(), Assignments: assignments, Events: rec, Clock: clk},
		ProposePathPlan: &usecases.ProposePathPlan{Events: rec, Clock: clk},
		AssignLabor:     &usecases.AssignLabor{Associates: associates, Assignments: assignments, Events: rec, Clock: clk, MaxHoursPerShift: maxEvalHours},
	}
	return wireSession(t, deps), rec
}

func assignedInitiator(t *testing.T, rec *recordingPublisher) shared.Initiator {
	t.Helper()
	if len(rec.got) != 1 {
		t.Fatalf("published %d events, want 1: %+v", len(rec.got), rec.got)
	}
	switch ev := rec.got[0].(type) {
	case shared.LaborAssigned:
		return ev.InitiatedBy
	case shared.LaborReassigned:
		return ev.InitiatedBy
	default:
		t.Fatalf("unexpected event %T", ev)
		return shared.Initiator{}
	}
}

func TestAssignLaborAttribution_StampsViaMCP(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want shared.Initiator
	}{
		{"no attribution still marks the surface", map[string]any{"associateId": "a1", "pathId": "pack"}, mustInit(t, "", "", "")},
		{"all three", map[string]any{"associateId": "a1", "pathId": "pack", "proposalId": "prop-9", "approvedBy": "sup-1", "agentId": "ops-agent"}, mustInit(t, "prop-9", "sup-1", "ops-agent")},
		{"values are trimmed", map[string]any{"associateId": "a1", "pathId": "pack", "proposalId": "  prop-9 ", "agentId": "\tops-agent"}, mustInit(t, "prop-9", "", "ops-agent")},
		{"maximum length accepted", map[string]any{"associateId": "a1", "pathId": "pack", "agentId": strings.Repeat("a", 128)}, mustInit(t, "", "", strings.Repeat("a", 128))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess, rec := newAttributionSession(t)
			res, err := sess.CallTool(t.Context(), &sdk.CallToolParams{Name: "assign_labor", Arguments: tc.args})
			if err != nil || res.IsError {
				t.Fatalf("assign_labor failed: err=%v res=%+v", err, res)
			}
			if got := assignedInitiator(t, rec); got != tc.want {
				t.Fatalf("initiator = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A move (the associate is already active) is attributed too.
func TestAssignLaborAttribution_ReassignmentIsAttributed(t *testing.T) {
	sess, rec := newAttributionSession(t)
	call := func(path string, extra map[string]any) {
		t.Helper()
		args := map[string]any{"associateId": "a1", "pathId": path}
		for k, v := range extra {
			args[k] = v
		}
		if res, err := sess.CallTool(t.Context(), &sdk.CallToolParams{Name: "assign_labor", Arguments: args}); err != nil || res.IsError {
			t.Fatalf("assign_labor(%s) failed: err=%v res=%+v", path, err, res)
		}
	}
	call("pack", nil)
	rec.got = nil
	call("pick", map[string]any{"proposalId": "prop-1", "approvedBy": "sup-1", "agentId": "ops-agent"})
	if got, want := assignedInitiator(t, rec), mustInit(t, "prop-1", "sup-1", "ops-agent"); got != want {
		t.Fatalf("initiator = %+v, want %+v", got, want)
	}
	if _, ok := rec.got[0].(shared.LaborReassigned); !ok {
		t.Fatalf("event = %T, want LaborReassigned", rec.got[0])
	}
}

// Strict decoding keeps refusing every other unknown name, and `via` is never
// a caller argument; a 129-character value is refused and nothing is written.
func TestAssignLaborAttribution_RejectsWhatItMust(t *testing.T) {
	cases := map[string]map[string]any{
		"via is not an argument":          {"associateId": "a1", "pathId": "pack", "via": "a2a"},
		"initiated_by is not an argument": {"associateId": "a1", "pathId": "pack", "initiated_by": "x"},
		"snake_case spelling is unknown":  {"associateId": "a1", "pathId": "pack", "proposal_id": "p"},
		"unrelated unknown name":          {"associateId": "a1", "pathId": "pack", "force": true},
		"proposalId over 128":             {"associateId": "a1", "pathId": "pack", "proposalId": strings.Repeat("p", 129)},
		"approvedBy over 128":             {"associateId": "a1", "pathId": "pack", "approvedBy": strings.Repeat("p", 129)},
		"agentId over 128":                {"associateId": "a1", "pathId": "pack", "agentId": strings.Repeat("p", 129)},
		"non-string proposalId":           {"associateId": "a1", "pathId": "pack", "proposalId": 7},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			sess, rec := newAttributionSession(t)
			res, err := sess.CallTool(t.Context(), &sdk.CallToolParams{Name: "assign_labor", Arguments: args})
			if err == nil && !res.IsError {
				t.Fatalf("expected a refusal, got %+v", res)
			}
			if len(rec.got) != 0 {
				t.Fatalf("a refused call published %+v", rec.got)
			}
		})
	}
}

// The three arguments are advertised as optional (not in `required`), and
// `via` is not advertised at all.
func TestAssignLaborAttribution_SchemaAdvertisesOptionalFieldsOnly(t *testing.T) {
	for _, tool := range wireTools(t) {
		if tool.Name != "assign_labor" {
			continue
		}
		s := schemaOf(t, tool.InputSchema)
		for _, name := range []string{"proposalId", "approvedBy", "agentId"} {
			if _, ok := s.Properties[name]; !ok {
				t.Errorf("assign_labor does not advertise %s", name)
			}
			for _, req := range s.Required {
				if req == name {
					t.Errorf("%s must be optional, but is required", name)
				}
			}
		}
		if _, ok := s.Properties["via"]; ok {
			t.Error("via must not be a caller argument")
		}
		return
	}
	t.Fatal("assign_labor not listed")
}

func mustInit(t *testing.T, proposal, approver, agent string) shared.Initiator {
	t.Helper()
	in, err := shared.NewInitiator(shared.ViaMCP, proposal, approver, agent)
	if err != nil {
		t.Fatal(err)
	}
	return in
}
