package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// Convergence on siteCode (decision 19, ADR 0035) over MCP: siteCode is the
// canonical argument, buildingId the deprecated alias with the same value.

func TestGetStaffingGap_SiteCodeCanonicalBuildingIdDeprecatedAlias(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedPlan(t, "WH1", "S1", "pack", 3)
	for id, site := range map[string]shared.SiteCode{"a1": "WH1", "a2": "WH1", "a3": "WH2", "a4": ""} {
		if _, err := h.start.ExecuteAtSite(ctx, shared.AssociateId(id), []shared.Certification{"pack"}, site); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if _, err := h.deps.AssignLabor.Execute(ctx, shared.AssociateId(id), "pack"); err != nil {
			t.Fatalf("assign %s: %v", id, err)
		}
	}

	tests := []struct {
		name       string
		in         staffingGapInput
		wantErr    bool
		wantActive int
		wantSite   string // echoed siteCode ("" = unscoped)
		wantKey    string // buildingId echo: the plan key, same value
	}{
		{"siteCode only: plan key AND scope", staffingGapInput{SiteCode: "WH1", ShiftId: "S1", PathId: "pack"}, false, 2, "WH1", "WH1"},
		{"buildingId only: deprecated alias, NOT scoped", staffingGapInput{BuildingId: "WH1", ShiftId: "S1", PathId: "pack"}, false, 4, "", "WH1"},
		{"both equal: scoped", staffingGapInput{SiteCode: "WH1", BuildingId: "WH1", ShiftId: "S1", PathId: "pack"}, false, 2, "WH1", "WH1"},
		{"neither is rejected", staffingGapInput{ShiftId: "S1", PathId: "pack"}, true, 0, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := h.deps.getStaffingGap(ctx, tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.ActiveHeads != tc.wantActive || out.SiteCode != tc.wantSite || out.BuildingId != tc.wantKey {
				t.Fatalf("got active=%d site=%q building=%q, want active=%d site=%q building=%q",
					out.ActiveHeads, out.SiteCode, out.BuildingId, tc.wantActive, tc.wantSite, tc.wantKey)
			}
		})
	}
}

func TestProposePathHeads_SiteCodeCanonicalBuildingIdDeprecatedAlias(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tests := []struct {
		name      string
		in        proposeHeadsInput
		wantErr   string // substring; "" = success
		wantKey   string
		wantHeads int
	}{
		{"siteCode only", proposeHeadsInput{SiteCode: "WH1", PathId: "pack", Charge: 100, PlannedRate: 30}, "", "WH1", 4},
		{"buildingId only (deprecated alias)", proposeHeadsInput{BuildingId: "B1", PathId: "pack", Charge: 100, PlannedRate: 30}, "", "B1", 4},
		{"both equal", proposeHeadsInput{SiteCode: "WH1", BuildingId: "WH1", PathId: "pack", Charge: 90, PlannedRate: 30}, "", "WH1", 3},
		{"both different is a conflict", proposeHeadsInput{SiteCode: "WH1", BuildingId: "B1", PathId: "pack", Charge: 100, PlannedRate: 30}, "different values", "", 0},
		{"neither", proposeHeadsInput{PathId: "pack", Charge: 100, PlannedRate: 30}, "siteCode is required", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := h.deps.proposePathHeads(ctx, tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.SiteCode != tc.wantKey || out.BuildingId != tc.wantKey || out.ProposedHeads != tc.wantHeads {
				t.Fatalf("got %+v, want siteCode=buildingId=%q heads=%d", out, tc.wantKey, tc.wantHeads)
			}
		})
	}
}

// The tool descriptions announce the deprecation and the canonical argument.
func TestTools_DescribeBuildingIdAsDeprecatedInFavourOfSiteCode(t *testing.T) {
	byName := map[string]*sdk.Tool{}
	for _, tool := range governanceTools(t) {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"get_staffing_gap", "propose_path_heads"} {
		tool := byName[name]
		if tool == nil {
			t.Fatalf("tool %q not advertised", name)
		}
		if !strings.Contains(tool.Description, "buildingId") || !strings.Contains(tool.Description, "deprecated") || !strings.Contains(tool.Description, "siteCode") {
			t.Errorf("%s description must say buildingId is deprecated in favour of siteCode, got %q", name, tool.Description)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal schema: %v", err)
		}
		var in struct {
			Required   []string                  `json:"required"`
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(schema, &in); err != nil {
			t.Fatalf("unmarshal schema: %v", err)
		}
		for _, r := range in.Required {
			if r == "buildingId" || r == "siteCode" {
				t.Errorf("%s: %s must not be individually required (one of the two is)", name, r)
			}
		}
		if d, _ := in.Properties["buildingId"]["description"].(string); !strings.Contains(strings.ToLower(d), "deprecated") {
			t.Errorf("%s: buildingId argument must be described as deprecated, got %q", name, d)
		}
		if _, ok := in.Properties["siteCode"]; !ok {
			t.Errorf("%s: canonical siteCode argument missing", name)
		}
	}
}

func readResource(t *testing.T, h *harness, uri string) (map[string]any, error) {
	t.Helper()
	server := NewServer(h.deps)
	client := sdk.NewClient(&sdk.Implementation{Name: "res", Version: "0"}, nil)
	ct, st := sdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, err
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &got); err != nil {
		t.Fatalf("decode resource: %v", err)
	}
	return got, nil
}

func TestStaffingGapResource_CanonicalSitesTemplateAndDeprecatedBuildingTemplate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedPlan(t, "WH1", "S1", "pack", 3)
	for id, site := range map[string]shared.SiteCode{"a1": "WH1", "a2": "WH2"} {
		if _, err := h.start.ExecuteAtSite(ctx, shared.AssociateId(id), []shared.Certification{"pack"}, site); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if _, err := h.deps.AssignLabor.Execute(ctx, shared.AssociateId(id), "pack"); err != nil {
			t.Fatalf("assign %s: %v", id, err)
		}
	}

	canonical, err := readResource(t, h, "staffing://sites/WH1/S1/pack/gap")
	if err != nil {
		t.Fatalf("canonical resource: %v", err)
	}
	if canonical["activeHeads"] != float64(1) || canonical["siteCode"] != "WH1" || canonical["buildingId"] != "WH1" {
		t.Fatalf("canonical sites template: plan key AND scope by siteCode, buildingId echo = same value, got %v", canonical)
	}

	legacy, err := readResource(t, h, "staffing://WH1/S1/pack/gap")
	if err != nil {
		t.Fatalf("deprecated resource must keep working: %v", err)
	}
	if legacy["activeHeads"] != float64(2) {
		t.Fatalf("the deprecated buildings template is not scoped by site (fleet-wide), got %v", legacy)
	}
	if _, scoped := legacy["siteCode"]; scoped {
		t.Fatalf("an unscoped resource must omit siteCode, got %v", legacy)
	}

	if _, err := readResource(t, h, "staffing://sites//S1/pack/gap"); err == nil {
		t.Fatal("an empty siteCode segment must be rejected")
	}
}

func TestParseStaffingSiteURI(t *testing.T) {
	site, shift, path, err := parseStaffingSiteURI("staffing://sites/WH1/S1/pack/gap")
	if err != nil || site != "WH1" || shift != "S1" || path != "pack" {
		t.Fatalf("got (%q,%q,%q,%v)", site, shift, path, err)
	}
	for _, bad := range []string{"staffing://WH1/S1/pack/gap", "staffing://sites/WH1/S1/gap", "staffing://sites/WH1/S1/pack/nope", "http://sites/WH1/S1/pack/gap"} {
		if _, _, _, err := parseStaffingSiteURI(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
