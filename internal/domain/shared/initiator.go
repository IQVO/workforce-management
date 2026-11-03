package shared

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Via names the agent surface a state change arrived through. It is stamped by
// the adapter that received the call, never supplied by the caller, so it
// cannot be spoofed by a tool argument.
type Via string

const (
	// ViaMCP marks a call received on the MCP inbound adapter.
	ViaMCP Via = "mcp"
	// ViaA2A marks a call received on the A2A inbound adapter.
	ViaA2A Via = "a2a"
)

// MaxInitiatorFieldLen is the longest accepted proposal id, approver or agent
// id, counted in characters after trimming.
const MaxInitiatorFieldLen = 128

// ErrInvalidInitiator is returned when an Initiator cannot be built: an unknown
// Via, or a caller-supplied field longer than MaxInitiatorFieldLen.
var ErrInvalidInitiator = errors.New("invalid initiator")

// Initiator records that a state change was initiated by an agent (Agentic
// Standard v2, attributable assignments) and who stands behind it: the
// proposal the agent acted on, the human who approved it, and the agent's own
// id. The zero Initiator means "not agent-initiated" (REST, internal callers)
// and is never published. It is a value object: immutable, compared by value.
//
// ProposalID, ApprovedBy and AgentID are free-form, caller-supplied
// attribution labels. They carry no authority: nothing in this context decides
// anything from them.
type Initiator struct {
	via        Via
	proposalID string
	approvedBy string
	agentID    string
}

// NewInitiator builds the Initiator for a call that arrived through via. The
// three attribution fields are optional; each is trimmed and an empty result
// is treated as absent.
func NewInitiator(via Via, proposalID, approvedBy, agentID string) (Initiator, error) {
	if via != ViaMCP && via != ViaA2A {
		return Initiator{}, fmt.Errorf("%w: via %q is not mcp or a2a", ErrInvalidInitiator, via)
	}
	fields := []struct{ name, value string }{
		{"proposalId", strings.TrimSpace(proposalID)},
		{"approvedBy", strings.TrimSpace(approvedBy)},
		{"agentId", strings.TrimSpace(agentID)},
	}
	for _, f := range fields {
		if n := utf8.RuneCountInString(f.value); n > MaxInitiatorFieldLen {
			return Initiator{}, fmt.Errorf("%w: %s is %d characters, the maximum is %d", ErrInvalidInitiator, f.name, n, MaxInitiatorFieldLen)
		}
	}
	return Initiator{via: via, proposalID: fields[0].value, approvedBy: fields[1].value, agentID: fields[2].value}, nil
}

// IsZero reports whether the change was not agent-initiated.
func (i Initiator) IsZero() bool { return i == Initiator{} }

// Via returns the agent surface the call arrived through ("" when zero).
func (i Initiator) Via() Via { return i.via }

// ProposalID returns the proposal the agent acted on ("" when absent).
func (i Initiator) ProposalID() string { return i.proposalID }

// ApprovedBy returns who approved the proposal ("" when absent).
func (i Initiator) ApprovedBy() string { return i.approvedBy }

// AgentID returns the acting agent's id ("" when absent).
func (i Initiator) AgentID() string { return i.agentID }
