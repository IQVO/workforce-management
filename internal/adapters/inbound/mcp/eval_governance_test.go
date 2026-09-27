// E1 — schema & metadata evals for the MCP tool surface.
//
// The governance tests police the charter's counting/naming rules; these
// evals go one level deeper: they prove every advertised tool's input
// schema is a resolvable, constraining JSON Schema (the thing an LLM host
// feeds a model), that every parameter carries a description, and that the
// advertised surface matches a golden registry shared across the fleet so
// tool names stay globally unique across the eight warehouse-systems
// servers. They run as a plain `go test` inside the existing CI test job —
// no new infrastructure.
package mcp_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// schemaOf extracts and parses the InputSchema of a wire-listed tool into
// the SDK's jsonschema type. Over Streamable HTTP the schema arrives as raw
// JSON exactly as a model host would see it.
func schemaOf(t *testing.T, raw any) *jsonschema.Schema {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(encoded, &s); err != nil {
		t.Fatalf("input schema is not valid JSON Schema (%v): %s", err, encoded)
	}
	return &s
}

// hasType reports whether a property declares the given JSON type, via
// either the single-type or multi-type form.
func hasType(s *jsonschema.Schema, typ string) bool {
	if s.Type == typ {
		return true
	}
	for _, t := range s.Types {
		if t == typ {
			return true
		}
	}
	return false
}

// validInstanceFor builds a schema-shaped arguments object from a tool's
// properties: strings become "probe", integers 1, numbers 1.5, booleans
// true, arrays and objects their empty forms. Good enough to prove the
// schema accepts what it declares. (Native Go numbers, not json.Number —
// the validator type-checks Go kinds and json.Number is a string kind.)
func validInstanceFor(s *jsonschema.Schema) map[string]any {
	instance := map[string]any{}
	for name, prop := range s.Properties {
		switch {
		case hasType(prop, "string"):
			instance[name] = "probe"
		case hasType(prop, "integer"):
			instance[name] = 1
		case hasType(prop, "number"):
			instance[name] = 1.5
		case hasType(prop, "boolean"):
			instance[name] = true
		case hasType(prop, "array"):
			instance[name] = []any{}
		case hasType(prop, "object"):
			instance[name] = map[string]any{}
		default:
			instance[name] = nil
		}
	}
	return instance
}

// TestEval_InputSchemasResolveAndConstrain proves, per advertised tool:
// (1) the input schema resolves (structurally valid, no dangling refs),
// (2) a schema-shaped arguments object validates cleanly, and
// (3) a wrong-typed value for a declared property is REJECTED — i.e. the
// schema genuinely constrains what a model may send, not just decorates it.
func TestEval_InputSchemasResolveAndConstrain(t *testing.T) {
	for _, tool := range wireTools(t) {
		t.Run(tool.Name, func(t *testing.T) {
			s := schemaOf(t, tool.InputSchema)
			if !hasType(s, "object") {
				t.Fatalf("input schema type = %q, want object", s.Type)
			}
			resolved, err := s.Resolve(nil)
			if err != nil {
				t.Fatalf("input schema does not resolve: %v", err)
			}

			valid := validInstanceFor(s)
			if err := resolved.Validate(valid); err != nil {
				t.Fatalf("schema rejects its own shape of arguments (%v): %v", valid, err)
			}

			// Flip the first string property to a number; the schema must
			// reject it. Tools without string properties skip this leg.
			// (float64, not json.Number — the validator type-checks Go
			// kinds, and json.Number is a string kind.)
			for name, prop := range s.Properties {
				if !hasType(prop, "string") {
					continue
				}
				wrong := map[string]any{name: float64(42)}
				if err := resolved.Validate(wrong); err == nil {
					t.Fatalf("schema accepts a numeric %q — it does not constrain model input", name)
				}
				break
			}
		})
	}
}

// TestEval_ParametersAreDescribed asserts every property of every tool
// schema carries a non-empty description, and that every required
// parameter is declared: these descriptions are the model UI, and a
// missing one silently degrades tool selection.
func TestEval_ParametersAreDescribed(t *testing.T) {
	for _, tool := range wireTools(t) {
		s := schemaOf(t, tool.InputSchema)
		for name, prop := range s.Properties {
			if strings.TrimSpace(prop.Description) == "" {
				t.Errorf("tool %q: parameter %q has no description", tool.Name, name)
			}
		}
		for _, name := range s.Required {
			if _, ok := s.Properties[name]; !ok {
				t.Errorf("tool %q: required parameter %q is not declared in properties", tool.Name, name)
			}
		}
	}
}

// TestEval_ToolRegistryMatchesGolden pins the exact advertised surface
// (names + write-intent annotations) to testdata/tool_registry.golden. Any
// addition, removal, or annotation change must be a conscious golden-file
// update reviewed against the charter's curation rules. The golden pins
// the DEFAULT deps surface; the curated report tool's conditional
// registration is pinned separately below.
func TestEval_ToolRegistryMatchesGolden(t *testing.T) {
	var got []string
	for _, tool := range wireTools(t) {
		destructive := false
		if tool.Annotations != nil && tool.Annotations.DestructiveHint != nil {
			destructive = *tool.Annotations.DestructiveHint
		}
		readOnly := false
		if tool.Annotations != nil {
			readOnly = tool.Annotations.ReadOnlyHint
		}
		got = append(got, fmt.Sprintf("%s\t%t\t%t", tool.Name, readOnly, destructive))
	}
	sort.Strings(got)

	goldenPath := filepath.Join("testdata", "tool_registry.golden")
	wantBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden registry: %v", err)
	}
	want := strings.Split(strings.TrimRight(string(wantBytes), "\n"), "\n")

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("advertised tool registry drifted from %s:\n want:\n%s\n got:\n%s",
			goldenPath, strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

// fleetRepos is the fixed set of warehouse-systems MCP servers the fleet
// snapshot may name. Anything else in the snapshot is a typo or a stale
// entry and fails the eval.
var fleetRepos = map[string]bool{
	"facility-layout":         true,
	"fulfillment-execution":   true,
	"inventory-storage":       true,
	"labor-performance":       true,
	"order-management":        true,
	"process-path-management": true,
	"wes-work-planning":       true,
	"workforce-management":    true,
}

// TestEval_FleetToolNamesAreGloballyUnique checks this server's tools
// against testdata/fleet_tool_snapshot.golden — the federated registry of
// every tool every fleet MCP server exposes (kept identical in all eight
// repos; each rollout PR appends its repo's section). A model host mounts
// several of these servers together, so a tool name must be globally
// unique, and this repo's registry must be fully represented in the
// snapshot.
func TestEval_FleetToolNamesAreGloballyUnique(t *testing.T) {
	snapshotBytes, err := os.ReadFile(filepath.Join("testdata", "fleet_tool_snapshot.golden"))
	if err != nil {
		t.Fatalf("read fleet snapshot: %v", err)
	}

	owner := map[string]string{} // tool name -> repo that owns it
	for lineNum, line := range strings.Split(strings.TrimRight(string(snapshotBytes), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			t.Fatalf("fleet snapshot line %d is not <repo>\\t<tool>: %q", lineNum+1, line)
		}
		repo, tool := parts[0], parts[1]
		if !fleetRepos[repo] {
			t.Fatalf("fleet snapshot names unknown repo %q (line %d)", repo, lineNum+1)
		}
		if other, clash := owner[tool]; clash {
			t.Fatalf("tool %q is exposed by both %s and %s — tool names must be globally unique across the fleet", tool, other, repo)
		}
		owner[tool] = repo
	}

	for _, tool := range wireTools(t) {
		if repo, ok := owner[tool.Name]; !ok {
			t.Errorf("tool %q is advertised but missing from fleet_tool_snapshot.golden — append it under workforce-management", tool.Name)
		} else if repo != "workforce-management" {
			t.Errorf("tool %q is advertised here but the fleet snapshot credits %s", tool.Name, repo)
		}
	}
}

// TestEval_ReportToolRegistrationIsConditional pins the one conditionally
// registered tool: get_workforce_labor_report exists only when a reports
// client is wired into Deps (an MCP deployment without the reports
// service). The goldens above pin the default surface; this eval keeps
// the conditional from silently drifting — with a client wired the surface
// is exactly the default three plus the report tool, and the report
// tool's schema meets the same bar as the always-registered tools.
func TestEval_ReportToolRegistrationIsConditional(t *testing.T) {
	defaultNames := map[string]bool{}
	for _, tool := range wireTools(t) {
		defaultNames[tool.Name] = true
		if tool.Name == "get_workforce_labor_report" {
			t.Fatalf("get_workforce_labor_report advertised without a reports client — the tool must be conditional")
		}
	}

	deps := newEvalDeps()
	deps.Reports = &fakeReportsClient{}
	var wired []*sdk.Tool
	sess := wireSession(t, deps)
	res, err := sess.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools with reports wired: %v", err)
	}
	wired = res.Tools

	if len(wired) != len(defaultNames)+1 {
		names := make([]string, 0, len(wired))
		for _, tool := range wired {
			names = append(names, tool.Name)
		}
		t.Fatalf("wired surface = %v, want the default surface plus get_workforce_labor_report", names)
	}
	for _, tool := range wired {
		if tool.Name == "get_workforce_labor_report" {
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("get_workforce_labor_report must be annotated read-only")
			}
			s := schemaOf(t, tool.InputSchema)
			if _, err := s.Resolve(nil); err != nil {
				t.Fatalf("report tool input schema does not resolve: %v", err)
			}
			for name, prop := range s.Properties {
				if strings.TrimSpace(prop.Description) == "" {
					t.Errorf("get_workforce_labor_report: parameter %q has no description", name)
				}
			}
			continue
		}
		if !defaultNames[tool.Name] {
			t.Errorf("wiring a reports client unexpectedly added tool %q", tool.Name)
		}
	}
}

// wireTools lists the advertised tools over the real Streamable HTTP
// handler with the DEFAULT deps (no reports client), so every eval sees
// exactly what a model host sees after a JSON round-trip (not the
// in-process Go values).
func wireTools(t *testing.T) []*sdk.Tool {
	t.Helper()
	sess := wireSession(t, newEvalDeps())
	res, err := sess.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	return res.Tools
}
