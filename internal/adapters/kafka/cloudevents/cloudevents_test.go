package cloudevents

import (
	"errors"
	"testing"
	"time"
)

func TestTypeAndDataSchema(t *testing.T) {
	if got, want := Type(EntityShiftPlan, "ShiftPlanCommitted"), TypeShiftPlanCommitted; got != want {
		t.Errorf("Type = %q, want %q", got, want)
	}
	cases := [][2]string{
		{Type(EntityShiftPlan, "ShiftPlanProposed"), TypeShiftPlanProposed},
		{Type(EntityShiftPlan, "PathUnderstaffed"), TypePathUnderstaffed},
		{Type(EntityAssociate, "AssociateShiftStarted"), TypeAssociateShiftStarted},
		{Type(EntityAssociate, "AssociateShiftEnded"), TypeAssociateShiftEnded},
		{Type(EntityAssociate, "AssociateBreakStarted"), TypeAssociateBreakStarted},
		{Type(EntityAssociate, "AssociateBreakEnded"), TypeAssociateBreakEnded},
		{Type(EntityAssociate, "AssociateCertified"), TypeAssociateCertified},
		{Type(EntityAssignment, "LaborAssigned"), TypeLaborAssigned},
		{Type(EntityAssignment, "LaborReassigned"), TypeLaborReassigned},
		{DataSchema(StreamEvents, "ShiftPlanCommitted", 1), "urn:warehouse:workforce-management:events:ShiftPlanCommitted:v1"},
		{DataSchema(StreamAnalytics, "LaborAssigned", 2), "urn:warehouse:workforce-management:analytics:LaborAssigned:v2"},
		{Source, "/warehouse/workforce-management"},
		{MediaType, "application/cloudevents+json; charset=UTF-8"},
		{string(ContentTypeHeader().Value), MediaType},
		{ContentTypeHeader().Key, "content-type"},
		{SpecVersion, "1.0"},
		{DataContentType, "application/json"},
		{TypeProcessPathCreated[:len("com.warehouse.wes.")], "com.warehouse.wes."},
	}
	for _, c := range cases {
		if got, want := c[0], c[1]; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestNew_GoldenJSON(t *testing.T) {
	b, err := New(Spec{
		ID:        "11111111-2222-4333-8444-555555555555",
		Entity:    EntityAssignment,
		EventName: "LaborAssigned",
		Subject:   "A-1",
		Time:      time.Date(2026, 9, 30, 12, 0, 0, 0, time.FixedZone("BRT", -3*3600)),
		Stream:    StreamAnalytics,
		Version:   1,
		Data:      map[string]any{"associate_id": "A-1", "path_id": "pack"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := `{"specversion":"1.0","id":"11111111-2222-4333-8444-555555555555","source":"/warehouse/workforce-management","type":"com.warehouse.wes.workforce-management.assignment.LaborAssigned","subject":"A-1","datacontenttype":"application/json","dataschema":"urn:warehouse:workforce-management:analytics:LaborAssigned:v1","time":"2026-09-30T15:00:00Z","data":{"associate_id":"A-1","path_id":"pack"}}`
	if string(b) != want {
		t.Errorf("golden mismatch\n got: %s\nwant: %s", b, want)
	}
}

func TestNew_RejectsEmptySubjectAndDefaultsVersion(t *testing.T) {
	if _, err := New(Spec{ID: "x", Entity: "e", EventName: "E", Time: time.Now(), Stream: StreamEvents, Data: map[string]any{}}); err == nil {
		t.Fatal("expected error for empty subject")
	}
	b, err := New(Spec{ID: "x", Entity: "e", EventName: "E", Subject: "s", Time: time.Now(), Stream: StreamEvents, Data: map[string]any{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if e.DataSchema() != "urn:warehouse:workforce-management:events:E:v1" {
		t.Errorf("dataschema = %q", e.DataSchema())
	}
}

func TestNew_RejectsEmptyID(t *testing.T) {
	if _, err := New(Spec{Entity: "e", EventName: "E", Subject: "s", Time: time.Now(), Stream: StreamEvents, Data: map[string]any{}}); err == nil {
		t.Fatal("expected validation error for empty id")
	}
}

func TestDecode_RoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	b, err := New(Spec{ID: "id-1", Entity: EntityShiftPlan, EventName: "ShiftPlanCommitted", Subject: "B/S", Time: at, Stream: StreamEvents, Version: 1, Data: map[string]any{"path_id": "pack"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if e.ID() != "id-1" || e.Type() != TypeShiftPlanCommitted || e.Subject() != "B/S" || !e.Time().Equal(at) || e.Source() != Source {
		t.Errorf("unexpected attributes: %+v", e)
	}
	var data struct {
		PathId string `json:"path_id"`
	}
	if err := e.DataAs(&data); err != nil || data.PathId != "pack" {
		t.Errorf("DataAs = %+v, %v", data, err)
	}
}

func TestDecode_RejectsLegacyAndGarbage(t *testing.T) {
	cases := map[string]string{
		"legacy flat envelope": `{"event_id":"e1","event_type":"ShiftPlanCommitted","occurred_at":"2026-08-21T22:00:00Z","source":"workforce-management","data":{"path_id":"pack"}}`,
		"legacy analytics":     `{"event_id":"e1","event_type":"LaborAssigned","occurred_at":"2026-08-21T22:00:00Z","source":"workforce-management","schema_version":1,"data":{}}`,
		"not json":             `{{{`,
		"wrong specversion":    `{"specversion":"0.3","id":"x","source":"/s","type":"t"}`,
		"missing id":           `{"specversion":"1.0","source":"/s","type":"t"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(raw))
			if !errors.Is(err, ErrNotCloudEvent) {
				t.Fatalf("Decode err = %v, want ErrNotCloudEvent", err)
			}
		})
	}
}
