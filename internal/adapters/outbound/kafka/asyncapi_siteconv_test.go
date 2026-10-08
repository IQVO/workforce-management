package kafka

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// The AsyncAPI contract must say what the publishers emit (ADR 0035): the plan
// events carry `site_code` (canonical) next to a deprecated `building_id`, and
// `site_code` is NOT required (messages produced before it existed lack it).
// planEventData is the `data` object of a plan event's CloudEvent schema.
type planEventData struct {
	Required   []string `yaml:"required"`
	Properties map[string]struct {
		Deprecated bool `yaml:"deprecated"`
	} `yaml:"properties"`
}

// planEventSchema is one components.schemas entry: an allOf whose parts may
// carry the `data` schema.
type planEventSchema struct {
	AllOf []struct {
		Properties struct {
			Data planEventData `yaml:"data"`
		} `yaml:"properties"`
	} `yaml:"allOf"`
}

func TestAsyncAPI_PlanEventsDeclareSiteCodeAndDeprecateBuildingId(t *testing.T) {
	raw, err := os.ReadFile("../../../../apis/asyncapi.yaml")
	if err != nil {
		t.Fatalf("read asyncapi: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]planEventSchema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse asyncapi: %v", err)
	}
	for _, name := range []string{"ShiftPlanCommittedEvent", "ShiftPlanCommittedAnalyticsEvent", "ShiftPlanProposedEvent"} {
		schema, ok := doc.Components.Schemas[name]
		if !ok {
			t.Fatalf("schema %s missing", name)
		}
		assertPlanEventDeclaresSiteCode(t, name, schema)
	}
}

// assertPlanEventDeclaresSiteCode checks one plan event's data schema: site_code
// is declared and optional, and building_id stays declared but deprecated.
func assertPlanEventDeclaresSiteCode(t *testing.T, name string, schema planEventSchema) {
	t.Helper()
	var found bool
	for _, part := range schema.AllOf {
		data := part.Properties.Data
		if len(data.Properties) == 0 {
			continue
		}
		found = true
		if _, ok := data.Properties["site_code"]; !ok {
			t.Errorf("%s: data.site_code is not declared", name)
		}
		if bid, ok := data.Properties["building_id"]; !ok || !bid.Deprecated {
			t.Errorf("%s: data.building_id must stay declared and be marked deprecated", name)
		}
		for _, r := range data.Required {
			if r == "site_code" {
				t.Errorf("%s: site_code must not be required (pre-existing outbox rows lack it)", name)
			}
		}
	}
	if !found {
		t.Errorf("%s: no data schema found", name)
	}
}
