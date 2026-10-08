package mcp

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// tracerName is the OTel instrumentation scope for MCP tool spans.
const tracerName = "github.com/claudioed/workforce-management/internal/adapters/inbound/mcp"

// Deps is everything the MCP tools need, injected by the composition root.
// It carries the same use cases the HTTP adapter uses; the adapter never
// constructs an outbound adapter itself.
type Deps struct {
	// GetStaffingGap is the existing read-model use case, reused unchanged.
	// It may raise PathUnderstaffed when a path is understaffed; that
	// behaviour is preserved — the tool simply reports the gap.
	GetStaffingGap *usecases.GetStaffingGap
	// ProposePathPlan is the existing pure-computation use case (heads needed
	// to cover a charge at a planned rate). It persists nothing.
	ProposePathPlan *usecases.ProposePathPlan
	// AssignLabor is the existing write use case, reused unchanged. Its domain
	// invariants (exactly one ACTIVE assignment per associate, certification
	// match required) make a model-invoked assignment safe by construction.
	AssignLabor *usecases.AssignLabor
	// Reports is the client of the workforce-reports REST service, backing the
	// curated get_workforce_labor_report tool. When nil, that tool is not
	// registered (an MCP deployment without the reports service).
	Reports ReportsClient
	// Catalogue validates every caller-supplied pathId against the fleet's
	// declared process-path catalogue (ADR-0013) before it reaches a use
	// case — exactly as the REST adapter does. ports.PathCatalogue (not the
	// concrete type) so a Kafka-fed catalogue can be wired in. A nil
	// Catalogue skips validation: that is a unit-test seam only; both
	// composition roots fail at boot when no catalogue can be loaded.
	Catalogue ports.PathCatalogue
}

// validatePathId checks pathId against the catalogue when one is wired in,
// mirroring the HTTP adapter's Handler.validatePathId.
func (d Deps) validatePathId(pathId string) error {
	if d.Catalogue == nil {
		return nil
	}
	if _, err := d.Catalogue.Lookup(pathId); err != nil {
		return fmt.Errorf("pathId %q: %w", pathId, err)
	}
	return nil
}

// --- get_staffing_gap ---------------------------------------------------------

type staffingGapInput struct {
	SiteCode   string `json:"siteCode,omitempty" jsonschema:"the canonical site code (the facility-layout Site code, e.g. WH1): the key of the committed shift plan AND the associate scope; when given, count only associates with an active shift at that site. Required unless the deprecated buildingId is given"`
	BuildingId string `json:"buildingId,omitempty" jsonschema:"DEPRECATED, use siteCode. The legacy name of the plan key (same value); a call giving only buildingId is not scoped by site and counts across every site"`
	ShiftId    string `json:"shiftId" jsonschema:"the shift whose committed plan to read the gap from"`
	PathId     string `json:"pathId" jsonschema:"the process path to measure planned-vs-active heads for (e.g. pack, pick, stow)"`
}

func (d Deps) getStaffingGap(ctx context.Context, in staffingGapInput) (staffingGap, error) {
	planKey, scope, err := shared.ResolveGapLookup(in.SiteCode, in.BuildingId)
	if err != nil {
		return staffingGap{}, err
	}
	if in.ShiftId == "" || in.PathId == "" {
		return staffingGap{}, fmt.Errorf("siteCode (or the deprecated buildingId), shiftId and pathId are required")
	}
	if err := d.validatePathId(in.PathId); err != nil {
		return staffingGap{}, err
	}
	gap, err := d.GetStaffingGap.ExecuteForSite(ctx, planKey, in.ShiftId, shared.PathId(in.PathId), scope)
	if err != nil {
		return staffingGap{}, err
	}
	return toStaffingGap(planKey, in.ShiftId, gap), nil
}

// --- propose_path_heads -------------------------------------------------------

type proposeHeadsInput struct {
	SiteCode    string  `json:"siteCode,omitempty" jsonschema:"the canonical site code (the facility-layout Site code, e.g. WH1) the proposal is for. Required unless the deprecated buildingId is given"`
	BuildingId  string  `json:"buildingId,omitempty" jsonschema:"DEPRECATED, use siteCode. Alias of siteCode with the same value; sending both with different values is rejected"`
	PathId      string  `json:"pathId" jsonschema:"the process path to size (e.g. pack, pick, stow)"`
	Charge      float64 `json:"charge" jsonschema:"the work charge (units) the path must clear this shift"`
	PlannedRate float64 `json:"plannedRate" jsonschema:"the planned rate (units per head) used to size headcount; must be greater than zero"`
}

type proposeHeadsOutput struct {
	SiteCode      string  `json:"siteCode"`
	BuildingId    string  `json:"buildingId"`
	PathId        string  `json:"pathId"`
	Charge        float64 `json:"charge"`
	PlannedRate   float64 `json:"plannedRate"`
	ProposedHeads int     `json:"proposedHeads"`
}

func (d Deps) proposePathHeads(ctx context.Context, in proposeHeadsInput) (proposeHeadsOutput, error) {
	siteKey, err := shared.ResolveSiteKey(in.SiteCode, in.BuildingId)
	if err != nil {
		return proposeHeadsOutput{}, err
	}
	if in.PathId == "" {
		return proposeHeadsOutput{}, fmt.Errorf("siteCode (or the deprecated buildingId) and pathId are required")
	}
	if err := d.validatePathId(in.PathId); err != nil {
		return proposeHeadsOutput{}, err
	}
	if in.Charge < 0 {
		return proposeHeadsOutput{}, fmt.Errorf("charge must not be negative")
	}
	if in.PlannedRate <= 0 {
		return proposeHeadsOutput{}, fmt.Errorf("plannedRate must be greater than zero")
	}
	// This tool requires plannedRate > 0 above, so ProposePathPlan never
	// consults the measured-rate fallback here; only rate-agnostic values
	// (heads) are relevant to this MCP tool's existing output contract.
	heads, _, _, _, err := d.ProposePathPlan.Execute(ctx, siteKey, shared.PathId(in.PathId), in.Charge, in.PlannedRate)
	if err != nil {
		return proposeHeadsOutput{}, err
	}
	return proposeHeadsOutput{
		SiteCode:      siteKey,
		BuildingId:    siteKey,
		PathId:        in.PathId,
		Charge:        in.Charge,
		PlannedRate:   in.PlannedRate,
		ProposedHeads: heads,
	}, nil
}

// --- assign_labor (write) -----------------------------------------------------

type assignLaborInput struct {
	AssociateId string `json:"associateId" jsonschema:"the associate to place on the path"`
	PathId      string `json:"pathId" jsonschema:"the process path to assign the associate to; the associate must hold the matching certification"`
}

func (d Deps) assignLabor(ctx context.Context, in assignLaborInput) (laborAssignmentView, error) {
	if in.AssociateId == "" || in.PathId == "" {
		return laborAssignmentView{}, fmt.Errorf("associateId and pathId are required")
	}
	if err := d.validatePathId(in.PathId); err != nil {
		return laborAssignmentView{}, err
	}
	la, err := d.AssignLabor.Execute(ctx, shared.AssociateId(in.AssociateId), shared.PathId(in.PathId))
	if err != nil {
		// The use case's domain errors (associate not found, lacks the required
		// certification, is on break, shift ended) surface unchanged as the tool
		// error; the single-active-assignment and certification-match invariants
		// make a mistaken model call safe.
		return laborAssignmentView{}, err
	}
	activePath, _ := la.ActivePathId()
	return laborAssignmentView{
		AssociateId: string(la.AssociateId()),
		PathId:      string(activePath),
		// LaborAssignment is keyed by associate (one active assignment per
		// associate is a structural invariant, ADR-0003), so the associate id
		// scoped by the active path is the assignment's stable identity.
		AssignmentId: fmt.Sprintf("%s@%s", la.AssociateId(), activePath),
	}, nil
}

// --- registration -------------------------------------------------------------

// registerTools adds every tool to the server, each wrapped so its handler
// runs inside an OTel span named "mcp.tool <name>".
func (d Deps) registerTools(server *mcp.Server) {
	readOnly := true

	addTool(server, &mcp.Tool{
		Name:        "get_staffing_gap",
		Description: "Return planned vs active heads for a process path within a site's committed shift plan, and whether it is understaffed. Identify the site with siteCode (the canonical argument: it is both the plan key and the associate scope); buildingId is deprecated in favour of siteCode, accepted as an alias for the plan key only and not scoped by site. Read-only; surfaces the gap, it does not move anyone.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly},
	}, d.getStaffingGap)

	addTool(server, &mcp.Tool{
		Name:        "propose_path_heads",
		Description: "Compute the headcount needed to cover a path's charge at a planned rate (ceil(charge/rate)) for a site (siteCode; buildingId is deprecated in favour of siteCode and is an alias with the same value). A pure proposal; it commits nothing and a human still commits the shift plan.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly},
	}, d.proposePathHeads)

	// Write tool: assigns an associate to a path. Annotated destructive
	// (non-read-only, non-idempotent) so a host can see it changes state
	// before letting a model call it. The domain invariants (one active
	// assignment per associate; certification match required) bound the
	// risk of a mistaken call.
	destructive := true
	notIdempotent := false
	addTool(server, &mcp.Tool{
		Name:        "assign_labor",
		Description: "Assign an associate to a process path, ending their prior active assignment if any. Rejected if the associate is unknown, lacks the path's required certification, is on break, or the shift has ended.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &destructive, IdempotentHint: notIdempotent},
	}, d.assignLabor)

	// Curated read-only data-product tool, registered only when the reports
	// client is configured.
	d.registerReportTool(server)
}

// addTool registers one tool. It centralises the cross-cutting concerns
// every tool shares: a span per call and mapping a handler error onto the
// span before returning it.
func addTool[In, Out any](
	server *mcp.Server,
	tool *mcp.Tool,
	handle func(context.Context, In) (Out, error),
) {
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		ctx, span := otel.Tracer(tracerName).Start(ctx, "mcp.tool "+tool.Name,
			trace.WithAttributes(
				attribute.String("mcp.tool.name", tool.Name),
			),
		)
		defer span.End()

		out, err := handle(ctx, in)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, zero, err
		}
		return nil, out, nil
	})
}
