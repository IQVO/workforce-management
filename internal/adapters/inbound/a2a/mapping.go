package a2a

import (
	"fmt"

	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// problemTypeBase namespaces this context's problem types, mirroring the
// RFC 7807 practice of the HTTP adapter (ADR-0005).
const problemTypeBase = "https://errors.workforce-management.warehouse-systems.dev"

// problem is the RFC 7807-shaped rejection an agent client receives instead
// of a result: domain rejections and request-shape failures are business
// answers, not transport errors, so they travel in the message envelope.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Status int    `json:"status"`
}

// summary renders the one-line form used for the reply's text part.
func (p *problem) summary() string {
	return fmt.Sprintf("%s (%d): %s", p.Title, p.Status, p.Detail)
}

func problemMalformedRequest(detail string) *problem {
	return &problem{Type: problemTypeBase + "/malformed-skill-request", Title: "Malformed skill request", Detail: detail, Status: 400}
}

func problemUnknownSkill(skill string) *problem {
	return &problem{Type: problemTypeBase + "/unknown-skill", Title: "Unknown skill", Detail: fmt.Sprintf("skill %q is not offered by this agent; the Agent Card lists the supported skills", skill), Status: 400}
}

func problemInvalidArgument(err error) *problem {
	return &problem{Type: problemTypeBase + "/invalid-argument", Title: "Invalid skill argument", Detail: err.Error(), Status: 400}
}

// envelope is the structured payload of every skill answer: exactly one of
// Data (a skill DTO) or Problem (why there is no result), plus the skill id
// the answer belongs to.
type envelope struct {
	Skill   string   `json:"skill"`
	Data    skillDTO `json:"data,omitempty"`
	Problem *problem `json:"problem,omitempty"`
}

// skillDTO is the union of the skill result DTOs. A2A parts are
// machine-readable JSON; each skill fills its own field and Summary, keeping
// the envelope flat and versioned as one contract.
type skillDTO struct {
	StaffingStatus *staffingStatus `json:"staffingStatus,omitempty"`
	LaborAssigned  *laborAssigned  `json:"laborAssigned,omitempty"`
	summary        string
}

// Summary is the human-readable one-liner for the reply's text part.
func (s skillDTO) Summary() string { return s.summary }

// staffingStatus is the composed read DTO for the staffing_status skill: the
// GetStaffingGap read model and, only when asked for, the ProposePathPlan
// sizing, side by side. Domain types never cross the boundary.
type staffingStatus struct {
	Gap         staffingGap  `json:"gap"`
	HeadsSizing *headsSizing `json:"headsSizing,omitempty"`
	summary     string
}

func (s staffingStatus) Summary() string { return s.summary }

// staffingGap mirrors the MCP tool DTO of the same read model (canonical
// siteCode only: the deprecated buildingId alias is not offered on this new
// surface).
type staffingGap struct {
	SiteCode     string `json:"siteCode"`
	ShiftId      string `json:"shiftId"`
	PathId       string `json:"pathId"`
	PlannedHeads int    `json:"plannedHeads"`
	ActiveHeads  int    `json:"activeHeads"`
	Understaffed bool   `json:"understaffed"`
}

// headsSizing is the optional sizing: the headcount needed to clear Charge at
// PlannedRate. It is computed by the existing pure use case and persists
// nothing; it is NOT a recommendation to act.
type headsSizing struct {
	Charge        float64 `json:"charge"`
	PlannedRate   float64 `json:"plannedRate"`
	ProposedHeads int     `json:"proposedHeads"`
}

// toStaffingStatus composes the read models into the skill DTO and its
// one-line summary.
func toStaffingStatus(siteCode, shiftId string, gap usecases.StaffingGap, sizing *headsSizing) staffingStatus {
	g := staffingGap{
		SiteCode:     siteCode,
		ShiftId:      shiftId,
		PathId:       string(gap.PathId),
		PlannedHeads: gap.PlannedHeads,
		ActiveHeads:  gap.ActiveHeads,
		Understaffed: gap.Understaffed,
	}
	state := "staffed"
	if g.Understaffed {
		state = "UNDERSTAFFED"
	}
	summary := fmt.Sprintf("%s/%s/%s: %d of %d planned heads active, %s", g.SiteCode, g.ShiftId, g.PathId, g.ActiveHeads, g.PlannedHeads, state)
	if sizing != nil {
		summary += fmt.Sprintf("; sizing: %d heads for charge %g at rate %g", sizing.ProposedHeads, sizing.Charge, sizing.PlannedRate)
	}
	return staffingStatus{Gap: g, HeadsSizing: sizing, summary: summary}
}

// laborAssigned is the write DTO for the assign_labor skill: who, which path,
// which assignment. The domain LaborAssignment aggregate never leaks across
// the boundary, exactly as on the MCP surface.
type laborAssigned struct {
	AssociateId  string `json:"associateId"`
	PathId       string `json:"pathId"`
	AssignmentId string `json:"assignmentId"`
	summary      string
}

func (l laborAssigned) Summary() string { return l.summary }

// toLaborAssigned maps the domain assignment into the DTO. The assignment is
// keyed by associate (one ACTIVE assignment per associate is structural,
// ADR-0003), so "<associate>@<path>" is its stable identity.
func toLaborAssigned(la *assignment.LaborAssignment) laborAssigned {
	activePath, _ := la.ActivePathId()
	dto := laborAssigned{
		AssociateId:  string(la.AssociateId()),
		PathId:       string(activePath),
		AssignmentId: fmt.Sprintf("%s@%s", la.AssociateId(), activePath),
	}
	dto.summary = fmt.Sprintf("assigned %s to %s", dto.AssociateId, dto.PathId)
	return dto
}

// siteCodeOf is a tiny helper so the skills file reads top to bottom.
func siteCodeOf(planKey string, scope shared.SiteCode) string {
	if scope != "" {
		return string(scope)
	}
	return planKey
}
