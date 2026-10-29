package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// tracerName is the OTel instrumentation scope for A2A skill spans, mirroring
// the MCP adapter's mcp.tool spans.
const tracerName = "github.com/claudioed/workforce-management/internal/adapters/inbound/a2a"

// Skill ids advertised on the Agent Card (see Card).
const (
	skillStaffingStatus = "staffing_status"
	skillAssignLabor    = "assign_labor"
)

// skillRequest is the wire contract of every skill call: the first meaningful
// message part carries (as a Data part, or a Text part holding the same JSON)
// the skill id and its arguments. Strict on purpose — an agent client is
// machine-to-machine, so unknown fields are rejected, never ignored: a
// misspelled name like "plannedRte" must not silently drop the sizing. (Go's
// encoding/json matches field names case-insensitively, so "plannedrate" still
// reaches plannedRate; only genuinely unknown names are refused.)
type skillRequest struct {
	Skill     string          `json:"skill"`
	Arguments json.RawMessage `json:"arguments"`
}

// staffingStatusArgs and assignLaborArgs are the per-skill argument shapes.
type staffingStatusArgs struct {
	SiteCode    string   `json:"siteCode"`
	ShiftId     string   `json:"shiftId"`
	PathId      string   `json:"pathId"`
	Charge      *float64 `json:"charge"`
	PlannedRate *float64 `json:"plannedRate"`
}

type assignLaborArgs struct {
	AssociateId string `json:"associateId"`
	PathId      string `json:"pathId"`
}

// answer maps one incoming message to a skill and returns the agent's reply
// Message. It is the whole executor body: this is a message-only agent, so a
// skill run is exactly one request mapped to one answer (ADR-0039).
func answer(deps Deps, ctx context.Context, ec *a2asrv.ExecutorContext) a2a.Event {
	req, problem := parseSkillRequest(ec)
	if problem != nil {
		return reply(ec, envelope{Skill: "", Problem: problem}, problem.summary())
	}

	ctx, span := otel.Tracer(tracerName).Start(ctx, "a2a.skill "+req.Skill)
	defer span.End()
	span.SetAttributes(attribute.String("a2a.skill", req.Skill))

	dto, problem := run(deps, ctx, req)
	if problem != nil {
		span.SetStatus(codes.Error, problem.Detail)
		return reply(ec, envelope{Skill: req.Skill, Problem: problem}, problem.summary())
	}
	return reply(ec, envelope{Skill: req.Skill, Data: dto}, dto.Summary())
}

// run dispatches one validated request to its skill.
func run(deps Deps, ctx context.Context, req skillRequest) (skillDTO, *problem) {
	switch req.Skill {
	case skillStaffingStatus:
		data, problem := deps.staffingStatus(ctx, req.Arguments)
		if problem != nil {
			return skillDTO{}, problem
		}
		return skillDTO{StaffingStatus: &data, summary: data.Summary()}, nil
	case skillAssignLabor:
		data, problem := deps.assignLabor(ctx, req.Arguments)
		if problem != nil {
			return skillDTO{}, problem
		}
		return skillDTO{LaborAssigned: &data, summary: data.Summary()}, nil
	default:
		return skillDTO{}, problemUnknownSkill(req.Skill)
	}
}

// reply builds the agent Message for one skill run: a Data part carrying the
// structured envelope and a Text part carrying the one-line summary, bound to
// the caller's context (and task, when the client referenced one).
func reply(ec *a2asrv.ExecutorContext, env envelope, summary string) a2a.Event {
	return a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewDataPart(env), a2a.NewTextPart(summary))
}

// parseSkillRequest extracts the skillRequest from the message's first
// meaningful part. A Data part (or a Text part holding the same JSON object)
// is accepted; anything else is a request-shape problem, answered — not
// raised — because a malformed delegation is a business answer for the
// calling agent, not a transport failure.
func parseSkillRequest(ec *a2asrv.ExecutorContext) (skillRequest, *problem) {
	msg := ec.Message
	if msg == nil || len(msg.Parts) == 0 {
		return skillRequest{}, problemMalformedRequest("the message carries no parts")
	}
	raw, err := partJSON(msg.Parts[0])
	if err != nil {
		return skillRequest{}, problemMalformedRequest(fmt.Sprintf("the first message part is not a skill request: %v", err))
	}
	var req skillRequest
	if err := decodeStrict(raw, &req); err != nil {
		return skillRequest{}, problemMalformedRequest(fmt.Sprintf("the skill request is not valid: %v", err))
	}
	if req.Skill == "" {
		return skillRequest{}, problemMalformedRequest(`the skill request carries no "skill" field`)
	}
	return req, nil
}

// partJSON renders one part's content as the JSON bytes of a skill request.
func partJSON(part *a2a.Part) ([]byte, error) {
	switch content := part.Content.(type) {
	case a2a.Data:
		return json.Marshal(content.Value)
	case a2a.Text:
		return []byte(string(content)), nil
	default:
		return nil, errors.New("expected a data part (or a text part holding the skill JSON)")
	}
}

// decodeStrict decodes one JSON document into dst, rejecting unknown fields
// and trailing data. Absent arguments decode as the empty object so each
// skill's own required-field checks produce the specific problem.
func decodeStrict(raw []byte, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON document")
	}
	return nil
}

// validatePathId checks pathId against the catalogue when one is wired in,
// mirroring the HTTP and MCP adapters. An unknown path is the caller's error.
func (d Deps) validatePathId(pathId string) error {
	if d.Catalogue == nil {
		return nil
	}
	if _, err := d.Catalogue.Lookup(pathId); err != nil {
		return fmt.Errorf("pathId %q: %w", pathId, err)
	}
	return nil
}

// staffingStatus is the composed read skill: the staffing gap for one path in
// one site's committed shift plan, plus the optional heads sizing, from this
// context's own state. The gap read may raise PathUnderstaffed on the way —
// unchanged, exactly as the HTTP and MCP surfaces see it.
func (d Deps) staffingStatus(ctx context.Context, rawArgs json.RawMessage) (staffingStatus, *problem) {
	var args staffingStatusArgs
	if err := decodeStrict(rawArgs, &args); err != nil {
		return staffingStatus{}, problemInvalidArgument(fmt.Errorf("arguments: %w", err))
	}
	planKey, scope, err := shared.ResolveGapLookup(args.SiteCode, "")
	if err != nil {
		return staffingStatus{}, problemFromUseCase(err)
	}
	if strings.TrimSpace(args.ShiftId) == "" || strings.TrimSpace(args.PathId) == "" {
		return staffingStatus{}, problemInvalidArgument(errors.New("siteCode, shiftId and pathId are required"))
	}
	if (args.Charge == nil) != (args.PlannedRate == nil) {
		return staffingStatus{}, problemInvalidArgument(errors.New("charge and plannedRate must be given together (or both omitted)"))
	}
	if args.Charge != nil && *args.Charge < 0 {
		return staffingStatus{}, problemInvalidArgument(errors.New("charge must not be negative"))
	}
	if args.PlannedRate != nil && *args.PlannedRate <= 0 {
		return staffingStatus{}, problemInvalidArgument(errors.New("plannedRate must be greater than zero"))
	}
	if err := d.validatePathId(args.PathId); err != nil {
		return staffingStatus{}, problemFromUseCase(err)
	}

	gap, err := d.GetStaffingGap.ExecuteForSite(ctx, planKey, args.ShiftId, shared.PathId(args.PathId), scope)
	if err != nil {
		return staffingStatus{}, problemFromUseCase(err)
	}
	var sizing *headsSizing
	if args.Charge != nil {
		heads, _, _, _, err := d.ProposePathPlan.Execute(ctx, planKey, shared.PathId(args.PathId), *args.Charge, *args.PlannedRate)
		if err != nil {
			return staffingStatus{}, problemFromUseCase(err)
		}
		sizing = &headsSizing{Charge: *args.Charge, PlannedRate: *args.PlannedRate, ProposedHeads: heads}
	}
	return toStaffingStatus(siteCodeOf(planKey, scope), args.ShiftId, gap, sizing), nil
}

// assignLabor is the single state-changing delegation: it assigns an
// associate through the same use case REST and MCP use, so the one-active-
// assignment and certification invariants and the transactional outbox
// behave identically regardless of caller.
func (d Deps) assignLabor(ctx context.Context, rawArgs json.RawMessage) (laborAssigned, *problem) {
	var args assignLaborArgs
	if err := decodeStrict(rawArgs, &args); err != nil {
		return laborAssigned{}, problemInvalidArgument(fmt.Errorf("arguments: %w", err))
	}
	if strings.TrimSpace(args.AssociateId) == "" || strings.TrimSpace(args.PathId) == "" {
		return laborAssigned{}, problemInvalidArgument(errors.New("associateId and pathId are required"))
	}
	if err := d.validatePathId(args.PathId); err != nil {
		return laborAssigned{}, problemFromUseCase(err)
	}
	la, err := d.AssignLabor.Execute(ctx, shared.AssociateId(args.AssociateId), shared.PathId(args.PathId))
	if err != nil {
		return laborAssigned{}, problemFromUseCase(err)
	}
	return toLaborAssigned(la), nil
}

// problemFromUseCase maps a use case error to a problem answer, using the
// SAME classification as the REST adapter (internal/adapters/inbound/http
// statusFor) so a delegating agent sees the outcome a REST caller would.
// Domain rejections are EXPECTED business outcomes and are answered as
// RFC 7807-shaped problems (ADR-0005); anything unrecognised is an internal
// failure, still answered as a problem — status 500 — so the calling agent
// always receives a structured, machine-readable result.
func problemFromUseCase(err error) *problem {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return &problem{Type: problemTypeBase + "/not-found", Title: "Resource not found", Detail: err.Error(), Status: 404}
	case errors.Is(err, shared.ErrEmptyAssociateId),
		errors.Is(err, shared.ErrEmptyPathId),
		errors.Is(err, shared.ErrEmptyCertification),
		errors.Is(err, shared.ErrMissingSiteKey),
		errors.Is(err, pathcatalog.ErrUnknownPath),
		errors.Is(err, shiftplan.ErrNoPathPlans),
		errors.Is(err, shiftplan.ErrMissingInstalledStations):
		return &problem{Type: problemTypeBase + "/validation", Title: "Request failed validation", Detail: err.Error(), Status: 400}
	case errors.Is(err, associate.ErrAlreadyOnBreak),
		errors.Is(err, associate.ErrNotOnBreak),
		errors.Is(err, associate.ErrOnBreak),
		errors.Is(err, associate.ErrShiftEnded),
		errors.Is(err, associate.ErrMaxHoursExceeded),
		errors.Is(err, assignment.ErrCertificationRequired),
		errors.Is(err, shiftplan.ErrPlannedHeadsExceedInstalled),
		errors.Is(err, shiftplan.ErrExceedsInstalledCapacity),
		errors.Is(err, shiftplan.ErrPlannedHoursExceedCapacity):
		return &problem{Type: problemTypeBase + "/business-rule", Title: "Request rejected by a domain rule", Detail: err.Error(), Status: 409}
	case errors.Is(err, ports.ErrConcurrentModification):
		return &problem{Type: problemTypeBase + "/concurrent-modification", Title: "Concurrent modification, nothing was written", Detail: err.Error(), Status: 409}
	case errors.Is(err, ports.ErrInstalledCapacityUnavailable):
		return &problem{Type: problemTypeBase + "/dependency-unavailable", Title: "A dependency could not be reached, retry later", Detail: err.Error(), Status: 503}
	default:
		slog.Error("a2a skill failed", "error", err)
		return &problem{Type: problemTypeBase + "/internal", Title: "Unexpected failure executing the skill", Detail: "the skill failed; see server logs (trace id in the response metadata)", Status: 500}
	}
}
