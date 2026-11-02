package a2a

import (
	"context"
	"strings"
	"testing"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// Agentic Standard v2 (AS-2): the assign_labor skill accepts the OPTIONAL
// proposalId / approvedBy / agentId, and the ADAPTER (never the caller) stamps
// via=a2a onto the event the use case publishes.

type recordingPublisher struct{ got []shared.DomainEvent }

func (p *recordingPublisher) Publish(_ context.Context, evts ...shared.DomainEvent) error {
	p.got = append(p.got, evts...)
	return nil
}

func newAttributionWire(t *testing.T) (*wire, *recordingPublisher) {
	t.Helper()
	h := newHarness(t, false)
	rec := &recordingPublisher{}
	associates := memory.NewAssociateRepo()
	assignments := memory.NewAssignmentRepo().WithAssociates(associates)
	start := &usecases.StartAssociateShift{Associates: associates, Events: rec, Clock: fixedClock{now: base}}
	if _, err := start.ExecuteAtSite(context.Background(), "a1", []shared.Certification{"pack", "pick"}, "WH1"); err != nil {
		t.Fatal(err)
	}
	rec.got = nil
	h.deps.AssignLabor = &usecases.AssignLabor{Associates: associates, Assignments: assignments, Events: rec, Clock: fixedClock{now: base}, MaxHoursPerShift: 10}
	return newWire(t, h.deps), rec
}

func initiatorOf(t *testing.T, rec *recordingPublisher) shared.Initiator {
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

func mustInit(t *testing.T, proposal, approver, agent string) shared.Initiator {
	t.Helper()
	in, err := shared.NewInitiator(shared.ViaA2A, proposal, approver, agent)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestWireAssignLaborAttribution_StampsViaA2A(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want shared.Initiator
	}{
		{"no attribution still marks the surface", map[string]any{"associateId": "a1", "pathId": "pack"}, mustInit(t, "", "", "")},
		{"all three", map[string]any{"associateId": "a1", "pathId": "pack", "proposalId": "prop-9", "approvedBy": "sup-1", "agentId": "ops-agent"}, mustInit(t, "prop-9", "sup-1", "ops-agent")},
		{"values are trimmed", map[string]any{"associateId": "a1", "pathId": "pack", "approvedBy": "  sup-1 "}, mustInit(t, "", "sup-1", "")},
		{"maximum length accepted", map[string]any{"associateId": "a1", "pathId": "pack", "proposalId": strings.Repeat("p", 128)}, mustInit(t, strings.Repeat("p", 128), "", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, rec := newAttributionWire(t)
			env, _ := w.skill(t, skillAssignLabor, tc.args)
			if env.Problem != nil {
				t.Fatalf("problem: %+v", env.Problem)
			}
			if got := initiatorOf(t, rec); got != tc.want {
				t.Fatalf("initiator = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestWireAssignLaborAttribution_ReassignmentIsAttributed(t *testing.T) {
	w, rec := newAttributionWire(t)
	if env, _ := w.skill(t, skillAssignLabor, map[string]any{"associateId": "a1", "pathId": "pack"}); env.Problem != nil {
		t.Fatalf("problem: %+v", env.Problem)
	}
	rec.got = nil
	env, _ := w.skill(t, skillAssignLabor, map[string]any{"associateId": "a1", "pathId": "pick", "proposalId": "prop-1", "agentId": "ops-agent"})
	if env.Problem != nil {
		t.Fatalf("problem: %+v", env.Problem)
	}
	if got, want := initiatorOf(t, rec), mustInit(t, "prop-1", "", "ops-agent"); got != want {
		t.Fatalf("initiator = %+v, want %+v", got, want)
	}
	if _, ok := rec.got[0].(shared.LaborReassigned); !ok {
		t.Fatalf("event = %T, want LaborReassigned", rec.got[0])
	}
}

func TestWireAssignLaborAttribution_RejectsWhatItMust(t *testing.T) {
	cases := map[string]map[string]any{
		"via is not an argument":          {"associateId": "a1", "pathId": "pack", "via": "mcp"},
		"initiated_by is not an argument": {"associateId": "a1", "pathId": "pack", "initiated_by": "x"},
		"snake_case spelling is unknown":  {"associateId": "a1", "pathId": "pack", "agent_id": "x"},
		"unrelated unknown name":          {"associateId": "a1", "pathId": "pack", "force": true},
		"proposalId over 128":             {"associateId": "a1", "pathId": "pack", "proposalId": strings.Repeat("p", 129)},
		"approvedBy over 128":             {"associateId": "a1", "pathId": "pack", "approvedBy": strings.Repeat("p", 129)},
		"agentId over 128":                {"associateId": "a1", "pathId": "pack", "agentId": strings.Repeat("p", 129)},
		"non-string agentId":              {"associateId": "a1", "pathId": "pack", "agentId": 7},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			w, rec := newAttributionWire(t)
			env, _ := w.skill(t, skillAssignLabor, args)
			wantProblem(t, env, 400, "invalid-argument")
			if len(rec.got) != 0 {
				t.Fatalf("a refused call published %+v", rec.got)
			}
		})
	}
}
