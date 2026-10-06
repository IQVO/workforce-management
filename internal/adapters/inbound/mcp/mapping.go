// Package mcp is the inbound Model Context Protocol adapter: it exposes this
// bounded context to the AI ecosystem as a second driving adapter over the
// same application-layer use cases the HTTP adapter uses. It is built on the
// official MCP Go SDK and served over Streamable HTTP.
//
// Per ADR-0008 and the MCP governance charter, this package depends inward on
// the application layer (use cases and ports) and the domain only — never on
// an outbound adapter. The composition root (cmd/mcp) wires concrete
// repositories into the use cases. Tool handlers call use cases; domain
// structs never leak across the tool boundary.
package mcp

import (
	"github.com/claudioed/workforce-management/internal/application/usecases"
)

// staffingGap is the tool-boundary DTO for a staffing-gap read. It mirrors
// the fields of the usecases.StaffingGap read model but is a plain, JSON-safe
// view: nothing but this file's DTOs crosses the tool boundary.
type staffingGap struct {
	BuildingId   string `json:"buildingId"`
	ShiftId      string `json:"shiftId"`
	PathId       string `json:"pathId"`
	PlannedHeads int    `json:"plannedHeads"`
	ActiveHeads  int    `json:"activeHeads"`
	Understaffed bool   `json:"understaffed"`
	// SiteCode is echoed only when the gap was scoped to one site (ADR 0034).
	SiteCode string `json:"siteCode,omitempty"`
}

// toStaffingGap maps the application read model into the tool-boundary DTO,
// carrying the request's building and shift back to the caller for context.
func toStaffingGap(buildingId, shiftId string, gap usecases.StaffingGap) staffingGap {
	return staffingGap{
		BuildingId:   buildingId,
		ShiftId:      shiftId,
		PathId:       string(gap.PathId),
		PlannedHeads: gap.PlannedHeads,
		ActiveHeads:  gap.ActiveHeads,
		Understaffed: gap.Understaffed,
		SiteCode:     string(gap.SiteCode),
	}
}

// laborAssignmentView is the tool-boundary DTO for a completed assignment. It
// is intentionally small (who, which path, which assignment) — the domain
// LaborAssignment aggregate never leaves the adapter.
type laborAssignmentView struct {
	AssociateId  string `json:"associateId"`
	PathId       string `json:"pathId"`
	AssignmentId string `json:"assignmentId"`
}
