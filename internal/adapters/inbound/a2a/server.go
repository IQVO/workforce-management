// Package a2a is the inbound Agent2Agent adapter: it exposes this bounded
// context to the fleet's agent ecosystem as a third driving adapter over the
// same application-layer use cases the HTTP and MCP adapters use. It is built
// on the official A2A Go SDK (a2aproject/a2a-go) and speaks JSON-RPC 2.0 over
// HTTP, with the Agent Card served at the well-known path.
//
// Per ADR-0039, A2A is the DELEGATION channel between bounded contexts: an
// agent client sends a goal-shaped skill request as a structured message part
// and this agent answers from its own state through its own use cases. It is
// not a data-sync channel — facts between contexts still travel exclusively
// as CloudEvents on Kafka (ADR-0026), including events raised by these skills
// through the transactional outbox.
//
// Like the MCP adapter (ADR-0008), this package depends inward on the
// application layer (use cases and ports) and the domain only — never on an
// outbound adapter. The composition root (cmd/a2a) wires concrete repositories
// into the use cases. Domain structs never leak across the message boundary.
package a2a

import (
	"context"
	"iter"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
)

// Config carries the deployment-specific identity of this agent: the absolute
// URL clients use to reach its JSON-RPC endpoint (advertised on the Agent
// Card) and the card version. It is supplied by the composition root from
// A2A_AGENT_URL; the card MUST advertise a URL the fleet's agent clients can
// actually dial (in-cluster: http://<release>-a2a.<ns>.svc.cluster.local:8093/).
type Config struct {
	AgentURL string
	Version  string
}

// Deps is everything the A2A skills need, injected by the composition root.
// It carries the same use cases the HTTP and MCP adapters use; the adapter
// never constructs an outbound adapter itself.
type Deps struct {
	// GetStaffingGap is the existing read-model use case, reused unchanged.
	// It may raise PathUnderstaffed when a path is understaffed; that
	// behaviour is preserved — the skill simply reports the gap.
	GetStaffingGap *usecases.GetStaffingGap
	// ProposePathPlan is the existing pure-computation use case (heads needed
	// to cover a charge at a planned rate). It persists nothing.
	ProposePathPlan *usecases.ProposePathPlan
	// AssignLabor is the existing write use case, reused unchanged. Its domain
	// invariants (exactly one ACTIVE assignment per associate, certification
	// match required) make a delegated assignment safe by construction.
	AssignLabor *usecases.AssignLabor
	// Catalogue validates every caller-supplied pathId against the fleet's
	// declared process-path catalogue (ADR-0013) before it reaches a use
	// case — exactly as the REST and MCP adapters do. A nil Catalogue skips
	// validation: a unit-test seam only; cmd/a2a fails at boot without one.
	Catalogue ports.PathCatalogue
}

// Card builds this agent's Agent Card (ADR-0039). The surface is two curated,
// goal-shaped skills — not one skill per endpoint, the same curation
// discipline ADR-0008 applied to MCP tools. staffing_status composes the two
// read models a delegating agent needs to answer "is this path staffed, and
// how many heads would it take"; assign_labor is the single state-changing
// delegation. The card declares no security schemes on purpose: this surface
// is unauthenticated by the same fleet-wide decision as REST and MCP;
// schemes are added here, not in a redesign, if that decision is ever
// reversed.
func Card(cfg Config) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        "workforce-management",
		Description: "Workforce Management agent for the Warehouse Execution System: answers whether a process path is staffed for a shift (planned vs active heads, with an optional headcount sizing for a charge at a planned rate) and assigns an associate to a path under this context's invariants (one active assignment per associate, certification match, no break, shift not ended). It makes the staffing gap legible and executes a delegated assignment; it never links an associate to a specific task.",
		Version:     cfg.Version,
		Provider: &a2a.AgentProvider{
			Org: "IQVO",
			URL: "https://github.com/IQVO/workforce-management",
		},
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(cfg.AgentURL, a2a.TransportProtocolJSONRPC),
		},
		DefaultInputModes:  []string{"application/json", "text/plain"},
		DefaultOutputModes: []string{"application/json", "text/plain"},
		Capabilities:       a2a.AgentCapabilities{Streaming: false},
		Skills: []a2a.AgentSkill{
			{
				ID:          skillStaffingStatus,
				Name:        "Staffing status for a process path",
				Description: "Composed read for one process path in one site's committed shift plan: planned vs active heads and whether the path is understaffed. When charge and plannedRate are both given, also the headcount needed to clear that charge at that rate (ceil(charge/rate)); the sizing persists nothing. Identify the site with siteCode (the canonical facility-layout Site code, e.g. WH1).",
				Tags:        []string{"workforce", "staffing", "read"},
				InputModes:  []string{"application/json"},
				Examples: []string{
					`{"skill":"staffing_status","arguments":{"siteCode":"WH1","shiftId":"shift-1","pathId":"PICK"}}`,
					`{"skill":"staffing_status","arguments":{"siteCode":"WH1","shiftId":"shift-1","pathId":"PICK","charge":120,"plannedRate":30}}`,
				},
			},
			{
				ID:          skillAssignLabor,
				Name:        "Assign an associate to a process path",
				Description: "Puts an associate on a process path, ending their prior active assignment if any, exactly as the REST and MCP surfaces do, and publishes LaborAssigned or LaborReassigned through the transactional outbox. Rejected (as a problem answer, nothing written) when the associate is unknown, lacks the path's required certification, is on break, or their shift has ended. NOT idempotent: each accepted call closes the prior interval and starts a new one, so a caller must not blindly retry an ambiguous failure.",
				Tags:        []string{"workforce", "assignment", "write"},
				InputModes:  []string{"application/json"},
				Examples: []string{
					`{"skill":"assign_labor","arguments":{"associateId":"assoc-1","pathId":"PICK"}}`,
				},
			},
		},
	}
}

// NewExecutor returns the AgentExecutor that maps an incoming message to a
// skill and answers with a single agent Message (a message-only agent in A2A
// terms): both curated skills are transactional request/response work, so
// there is no long-running Task lifecycle to track in this first A2A
// surface. The response carries the caller's contextId (and taskId when the
// client referenced one) so a delegating saga can group its interactions.
func NewExecutor(deps Deps) a2asrv.AgentExecutor {
	return a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(answer(deps, ctx, ec), nil)
		}
	})
}

// Handler wires the executor into the transport-agnostic A2A request handler
// and wraps it in the JSON-RPC 2.0 HTTP handler — the only transport this
// agent ships (ADR-0039), mirroring ADR-0008's "Streamable HTTP only"
// minimalism for MCP.
func Handler(executor a2asrv.AgentExecutor) http.Handler {
	return a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor))
}
