package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// registerResources adds the scoped read-model resources. Per the charter,
// resources are bounded-context contracts tied to a decision, not bulk dumps:
// the staffing-gap resource answers "how does one path stand against its
// committed plan?" for a specific site/shift/path, backed by the same
// GetStaffingGap read model the tool uses.
//
// Two URI templates (ADR 0035):
//
//   - staffing://sites/{siteCode}/{shiftId}/{pathId}/gap — CANONICAL. The
//     siteCode is both the plan key and the associate scope.
//   - staffing://{buildingId}/{shiftId}/{pathId}/gap — DEPRECATED alias of the
//     plan key; unscoped, behaviour identical to before the convergence.
//
// The concrete values are read from the request URI at call time.
func (d Deps) registerResources(server *mcp.Server) {
	const siteTemplate = "staffing://sites/{siteCode}/{shiftId}/{pathId}/gap"
	server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: siteTemplate,
		Name:        "staffing gap",
		Description: "Planned-vs-active heads and understaffed flag for one process path within a site's committed shift plan; siteCode is both the plan key and the associate scope.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		siteCode, shiftId, pathId, err := parseStaffingSiteURI(req.Params.URI)
		if err != nil {
			return nil, err
		}
		gap, err := d.GetStaffingGap.ExecuteForSite(ctx, siteCode, shiftId, shared.PathId(pathId), shared.NewSiteCode(siteCode))
		if err != nil {
			return nil, err
		}
		return staffingResourceResult(req.Params.URI, toStaffingGap(siteCode, shiftId, gap))
	})

	const buildingTemplate = "staffing://{buildingId}/{shiftId}/{pathId}/gap"
	server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: buildingTemplate,
		Name:        "staffing gap (buildingId, deprecated)",
		Description: "DEPRECATED, use staffing://sites/{siteCode}/{shiftId}/{pathId}/gap: buildingId is the legacy name of the plan key (same value) and this template is not scoped by site.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		buildingId, shiftId, pathId, err := parseStaffingURI(req.Params.URI)
		if err != nil {
			return nil, err
		}
		gap, err := d.GetStaffingGap.Execute(ctx, buildingId, shiftId, shared.PathId(pathId))
		if err != nil {
			return nil, err
		}
		return staffingResourceResult(req.Params.URI, toStaffingGap(buildingId, shiftId, gap))
	})
}

func staffingResourceResult(uri string, gap staffingGap) (*mcp.ReadResourceResult, error) {
	body, err := json.Marshal(gap)
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(body),
		}},
	}, nil
}

// parseStaffingURI extracts buildingId, shiftId and pathId from a
// staffing://{buildingId}/{shiftId}/{pathId}/gap URI. The three path segments
// are model-supplied and therefore untrusted; a malformed URI or an empty
// segment is rejected rather than silently defaulted.
func parseStaffingURI(uri string) (buildingId, shiftId, pathId string, err error) {
	const scheme = "staffing://"
	rest, ok := trimPrefix(uri, scheme)
	if !ok {
		return "", "", "", fmt.Errorf("resource uri %q must start with %q", uri, scheme)
	}
	parts := splitSlash(rest)
	if len(parts) != 4 || parts[3] != "gap" {
		return "", "", "", fmt.Errorf("resource uri %q must be staffing://{buildingId}/{shiftId}/{pathId}/gap", uri)
	}
	buildingId, shiftId, pathId = parts[0], parts[1], parts[2]
	if buildingId == "" || shiftId == "" || pathId == "" {
		return "", "", "", fmt.Errorf("resource uri %q has an empty buildingId, shiftId or pathId segment", uri)
	}
	return buildingId, shiftId, pathId, nil
}

// parseStaffingSiteURI extracts siteCode, shiftId and pathId from the
// canonical staffing://sites/{siteCode}/{shiftId}/{pathId}/gap URI, with the
// same untrusted-input discipline as parseStaffingURI.
func parseStaffingSiteURI(uri string) (siteCode, shiftId, pathId string, err error) {
	const scheme = "staffing://sites/"
	rest, ok := trimPrefix(uri, scheme)
	if !ok {
		return "", "", "", fmt.Errorf("resource uri %q must start with %q", uri, scheme)
	}
	parts := splitSlash(rest)
	if len(parts) != 4 || parts[3] != "gap" {
		return "", "", "", fmt.Errorf("resource uri %q must be staffing://sites/{siteCode}/{shiftId}/{pathId}/gap", uri)
	}
	siteCode, shiftId, pathId = parts[0], parts[1], parts[2]
	if siteCode == "" || shiftId == "" || pathId == "" {
		return "", "", "", fmt.Errorf("resource uri %q has an empty siteCode, shiftId or pathId segment", uri)
	}
	return siteCode, shiftId, pathId, nil
}

func trimPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}

func splitSlash(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
