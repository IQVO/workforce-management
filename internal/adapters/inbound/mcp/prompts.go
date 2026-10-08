package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// coverStaffingGapsSOP is the operational standard-operating-procedure the
// cover_staffing_gaps prompt hands to the model. Per the charter, prompts
// encode how to interpret results, when to escalate, and what "done" means —
// they standardise agent behaviour across clients rather than leaving
// procedure implicit.
const coverStaffingGapsSOP = `You are helping cover staffing gaps in a warehouse shift. Use only the MCP tools; never assume state. This context surfaces the gap and enforces the invariants — the decision to move a person is a human call, so recommend, do not act autonomously beyond a clearly-safe assignment.

Procedure:
1. For each process path you care about, call get_staffing_gap(siteCode, shiftId, pathId). siteCode is the canonical site identifier (the plan key and the associate scope); buildingId is a deprecated alias of the plan key and is not scoped by site. A path is understaffed when activeHeads < plannedHeads; the shortfall is plannedHeads - activeHeads.
2. Rank paths by shortfall, largest first — that is where coverage is most urgent.
3. If you are sizing a plan rather than reading one, call propose_path_heads(siteCode, pathId, charge, plannedRate) to see how many heads a path needs at a given rate. This proposes only; it commits nothing.
4. To actually move a certified, available associate onto an understaffed path, call assign_labor(associateId, pathId). This ends the associate's prior active assignment and starts the new one. It is rejected if the associate lacks the path's required certification, is on break, or their shift has ended — treat that rejection as a hard stop, not something to work around.

Interpretation:
- A large, persistent shortfall on a path is a coverage problem: not enough certified heads assigned.
- A rejection from assign_labor is the domain protecting an invariant (certification match, single active assignment). Report it; do not retry with a different associate blindly.

Escalate to a human when: a path stays understaffed after the certified, available associates are exhausted, or when covering one path would strand another. Moving people is a human call — this context makes the gap legible, it does not decide.

Done means: for each path you were asked about you have reported planned vs active heads and the shortfall, proposed or performed any clearly-safe assignment, and surfaced any path still understaffed with a one-line reason.`

// registerPrompts adds the workflow prompts (operational SOPs).
func (d Deps) registerPrompts(server *mcp.Server) {
	server.AddPrompt(&mcp.Prompt{
		Name:        "cover_staffing_gaps",
		Description: "Standard operating procedure for reading staffing gaps and covering understaffed paths with certified, available associates.",
	}, func(ctx context.Context, _ *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Description: "How to cover staffing gaps: read planned-vs-active heads, rank shortfalls, propose or make a safe assignment, and know when to escalate.",
			Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: coverStaffingGapsSOP},
			}},
		}, nil
	})
}
