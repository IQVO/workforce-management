package kafka

// Consumer contract tests (E1-E4) for the warehouse-planning edge (ADR 0043):
// LabourPlanConsumer reads LabourPlanTopic (warehouse.warehouse-planning.events)
// and acts on the full CloudEvents type cloudevents.TypeLabourPlanCommitted only. The
// contract is the PINNED copy of warehouse-planning's own apis/asyncapi.yaml
// (testdata/warehouse-planning.asyncapi.yaml, registered in
// contracts/pinned.json; the pinned-contracts CI job fails when the copy drifts
// from IQVO/warehouse-planning@develop). warehouse-planning's file documents
// its own integration topic under 'subscribe:'.
//
// Golden fixtures are the producer spec's OWN examples, never hand-written.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
)

const pinnedWarehousePlanningSpec = "warehouse-planning.asyncapi.yaml"

type pinnedOperation struct {
	Message struct {
		Ref   string `yaml:"$ref"`
		OneOf []struct {
			Ref string `yaml:"$ref"`
		} `yaml:"oneOf"`
	} `yaml:"message"`
}

type pinnedSpec struct {
	Channels map[string]struct {
		Subscribe *pinnedOperation `yaml:"subscribe"`
	} `yaml:"channels"`
	Components struct {
		Messages map[string]struct {
			Payload  map[string]any `yaml:"payload"`
			Examples []struct {
				Name    string         `yaml:"name"`
				Payload map[string]any `yaml:"payload"`
			} `yaml:"examples"`
		} `yaml:"messages"`
		Schemas map[string]map[string]any `yaml:"schemas"`
	} `yaml:"components"`
}

func loadPinnedWarehousePlanning(t *testing.T) *pinnedSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", pinnedWarehousePlanningSpec))
	if err != nil {
		t.Fatal(err)
	}
	var s pinnedSpec
	if err := yaml.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse pinned spec: %v", err)
	}
	return &s
}

// channelMessages returns the component message names on the 'subscribe'
// operation of a channel (where warehouse-planning documents its own topics).
func (s *pinnedSpec) channelMessages(t *testing.T, channel string) []string {
	t.Helper()
	op := s.Channels[channel].Subscribe
	if op == nil {
		t.Fatalf("pinned spec has no subscribe operation on %s", channel)
	}
	m := op.Message
	refs := []string{m.Ref}
	for _, r := range m.OneOf {
		refs = append(refs, r.Ref)
	}
	var names []string
	for _, r := range refs {
		if r != "" {
			names = append(names, r[strings.LastIndex(r, "/")+1:])
		}
	}
	if len(names) == 0 {
		t.Fatalf("pinned spec documents no messages on %s", channel)
	}
	return names
}

// resolve follows $ref chains and merges allOf members into one property map
// and one required list.
func (s *pinnedSpec) resolve(node map[string]any) (map[string]any, []string) {
	if ref, ok := node["$ref"].(string); ok {
		return s.resolve(s.Components.Schemas[ref[strings.LastIndex(ref, "/")+1:]])
	}
	props := map[string]any{}
	var required []string
	for _, member := range asSlice(node["allOf"]) {
		if m, ok := member.(map[string]any); ok {
			p, r := s.resolve(m)
			for k, v := range p {
				props[k] = v
			}
			required = append(required, r...)
		}
	}
	if p, ok := node["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = v
		}
	}
	for _, r := range asSlice(node["required"]) {
		if name, ok := r.(string); ok {
			required = append(required, name)
		}
	}
	return props, required
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// goldenBytes encodes a spec example payload (a structured CloudEvents
// document) as the JSON wire value.
func goldenBytes(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode golden example: %v", err)
	}
	return raw
}

// goldenBytesWith encodes a deep copy of the payload after edit changed its data.
func goldenBytesWith(t *testing.T, payload map[string]any, edit func(data map[string]any)) []byte {
	t.Helper()
	var cp map[string]any
	if err := json.Unmarshal(goldenBytes(t, payload), &cp); err != nil {
		t.Fatal(err)
	}
	data, ok := cp["data"].(map[string]any)
	if !ok {
		t.Fatal("golden example has no data object")
	}
	edit(data)
	return goldenBytes(t, cp)
}

func (s *pinnedSpec) typeConst(t *testing.T, message string) string {
	t.Helper()
	props, _ := s.resolve(s.Components.Messages[message].Payload)
	typ, _ := props["type"].(map[string]any)
	c, _ := typ["const"].(string)
	return c
}

// E1: the topic LabourPlanConsumer reads is documented by the producer, and
// its message list carries cloudevents.TypeLabourPlanCommitted; LabourPlanDriftDetected is
// documented on the same channel and deliberately not routed.
func TestPinnedAsyncAPICoversSubscriptions_WarehousePlanning(t *testing.T) {
	spec := loadPinnedWarehousePlanning(t)
	documented := map[string]bool{}
	for _, name := range spec.channelMessages(t, LabourPlanTopic) {
		documented[spec.typeConst(t, name)] = true
	}
	if !documented[cloudevents.TypeLabourPlanCommitted] {
		t.Fatalf("pinned warehouse-planning spec does not document %s on %s (documented: %v)", cloudevents.TypeLabourPlanCommitted, LabourPlanTopic, documented)
	}
	if !documented["com.warehouse.wes.warehouse-planning.labourplan.LabourPlanDriftDetected"] {
		t.Errorf("pinned spec no longer documents LabourPlanDriftDetected on %s; revisit the ignore list", LabourPlanTopic)
	}
}

// E2: every LabourPlanCommitted example lands in the copy through the REAL
// consumer, committed and not dead-lettered, and its redelivery saves nothing.
func TestGoldenEventsDecode_WarehousePlanning(t *testing.T) {
	spec := loadPinnedWarehousePlanning(t)
	examples := spec.Components.Messages["LabourPlanCommitted"].Examples
	if len(examples) == 0 {
		t.Fatal("pinned spec documents LabourPlanCommitted without examples")
	}
	for _, ex := range examples {
		t.Run(ex.Name, func(t *testing.T) {
			h := newHarness(t)
			raw := goldenBytes(t, ex.Payload)
			for range 2 {
				if err := h.consumer.handleFetched(context.Background(), message(raw)); err != nil {
					t.Fatal(err)
				}
			}
			if h.copy.saves != 1 || len(h.reader.committed) != 2 || len(h.dlq.written) != 0 {
				t.Fatalf("saves %d, committed %d, dead-lettered %d; want 1, 2, 0", h.copy.saves, len(h.reader.committed), len(h.dlq.written))
			}
		})
	}
}

// E3: everything the consumer requires is REQUIRED by the producer's data
// schema; shift_start stays optional (the consumer never needs it).
func TestDataSchemaRequiresWhatTheConsumerNeeds_WarehousePlanning(t *testing.T) {
	spec := loadPinnedWarehousePlanning(t)
	props, _ := spec.resolve(spec.Components.Messages["LabourPlanCommitted"].Payload)
	data, ok := props["data"].(map[string]any)
	if !ok {
		t.Fatal("LabourPlanCommitted payload has no data schema")
	}
	_, required := spec.resolve(data)
	for _, field := range []string{"site_id", "shift_id", "path_id", "planned_heads", "rate_units_per_hour", "hours", "heads_source", "version"} {
		if !slices.Contains(required, field) {
			t.Errorf("producer does not require data.%s (required: %v)", field, required)
		}
	}
	if slices.Contains(required, "shift_start") {
		t.Error("producer made shift_start required; the consumer treats it as optional")
	}
}

// E4: additive data attributes are tolerated, and the producer's own
// LabourPlanDriftDetected example on the shared topic is skipped (committed,
// nothing saved, nothing dead-lettered).
func TestUnknownAttributesTolerated_WarehousePlanning(t *testing.T) {
	spec := loadPinnedWarehousePlanning(t)
	golden := spec.Components.Messages["LabourPlanCommitted"].Examples[0].Payload
	h := newHarness(t)
	tolerant := goldenBytesWith(t, golden, func(data map[string]any) { data["future_additive_field"] = "x" })
	if err := h.consumer.handleFetched(context.Background(), message(tolerant)); err != nil {
		t.Fatal(err)
	}
	drift := spec.Components.Messages["LabourPlanDriftDetected"].Examples
	if len(drift) == 0 {
		t.Fatal("pinned spec documents LabourPlanDriftDetected without examples")
	}
	if err := h.consumer.handleFetched(context.Background(), message(goldenBytes(t, drift[0].Payload))); err != nil {
		t.Fatal(err)
	}
	if h.copy.saves != 1 || len(h.reader.committed) != 2 || len(h.dlq.written) != 0 {
		t.Fatalf("saves %d, committed %d, dead-lettered %d; want 1, 2, 0", h.copy.saves, len(h.reader.committed), len(h.dlq.written))
	}
}
