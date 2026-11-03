package shared

import (
	"errors"
	"strings"
	"testing"
)

func TestNewInitiator(t *testing.T) {
	long := strings.Repeat("x", MaxInitiatorFieldLen)
	cases := []struct {
		name                       string
		via                        Via
		proposal, approver, agent  string
		wantErr                    bool
		wantProposal, wantApprover string
		wantAgent                  string
	}{
		{name: "mcp with nothing else", via: ViaMCP},
		{name: "a2a with all three", via: ViaA2A, proposal: "p-1", approver: "sup-7", agent: "ops-agent",
			wantProposal: "p-1", wantApprover: "sup-7", wantAgent: "ops-agent"},
		{name: "fields are trimmed", via: ViaMCP, proposal: "  p-1\t", approver: " sup ", agent: "\nag ",
			wantProposal: "p-1", wantApprover: "sup", wantAgent: "ag"},
		{name: "blank after trim is absent", via: ViaMCP, proposal: "   ", approver: "", agent: "\t"},
		{name: "exactly the maximum is accepted", via: ViaMCP, proposal: long, wantProposal: long},
		{name: "one over the maximum is refused (proposal)", via: ViaMCP, proposal: long + "x", wantErr: true},
		{name: "one over the maximum is refused (approver)", via: ViaA2A, approver: long + "x", wantErr: true},
		{name: "one over the maximum is refused (agent)", via: ViaA2A, agent: long + "x", wantErr: true},
		{name: "padding does not count toward the maximum", via: ViaMCP, proposal: "  " + long + "  ", wantProposal: long},
		{name: "the maximum counts characters, not bytes", via: ViaMCP, proposal: strings.Repeat("é", MaxInitiatorFieldLen), wantProposal: strings.Repeat("é", MaxInitiatorFieldLen)},
		{name: "unknown via is refused", via: "rest", wantErr: true},
		{name: "empty via is refused", via: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := NewInitiator(tc.via, tc.proposal, tc.approver, tc.agent)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidInitiator) {
					t.Fatalf("err = %v, want ErrInvalidInitiator", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if in.IsZero() {
				t.Fatal("an agent-initiated Initiator must not be zero")
			}
			if in.Via() != tc.via || in.ProposalID() != tc.wantProposal || in.ApprovedBy() != tc.wantApprover || in.AgentID() != tc.wantAgent {
				t.Fatalf("initiator = %v/%q/%q/%q, want %v/%q/%q/%q", in.Via(), in.ProposalID(), in.ApprovedBy(), in.AgentID(),
					tc.via, tc.wantProposal, tc.wantApprover, tc.wantAgent)
			}
		})
	}
}

func TestZeroInitiatorIsNotAgentInitiated(t *testing.T) {
	var in Initiator
	if !in.IsZero() {
		t.Fatal("the zero Initiator must report IsZero")
	}
	if in.Via() != "" || in.ProposalID() != "" || in.ApprovedBy() != "" || in.AgentID() != "" {
		t.Fatalf("zero Initiator exposes data: %+v", in)
	}
}

func TestLaborEventsCarryTheInitiator(t *testing.T) {
	in, err := NewInitiator(ViaA2A, "p-1", "sup", "ag")
	if err != nil {
		t.Fatal(err)
	}
	if got := NewLaborAssignedBy(fixedTime, "a1", "pack", in).InitiatedBy; got != in {
		t.Errorf("LaborAssigned.InitiatedBy = %+v, want %+v", got, in)
	}
	if got := NewLaborReassignedBy(fixedTime, "a1", "pack", "pick", in).InitiatedBy; got != in {
		t.Errorf("LaborReassigned.InitiatedBy = %+v, want %+v", got, in)
	}
	if !NewLaborAssigned(fixedTime, "a1", "pack").InitiatedBy.IsZero() {
		t.Error("NewLaborAssigned must leave the event not agent-initiated")
	}
	if !NewLaborReassigned(fixedTime, "a1", "pack", "pick").InitiatedBy.IsZero() {
		t.Error("NewLaborReassigned must leave the event not agent-initiated")
	}
}
