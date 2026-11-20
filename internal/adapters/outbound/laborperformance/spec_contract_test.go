package laborperformance

// Consumer contract tests against the PINNED COPY of labor-performance's
// published spec (testdata/labor-performance.openapi.yaml, pinned from
// IQVO/labor-performance apis/openapi.yaml@develop in contracts/pinned.json
// and kept equal by the pinned-contracts CI job; refresh = copy, never edit).
//
// Archetypes as in the fulfillment-execution contract test (R1 route, R2
// fixture, R3 gateway, R4 DTO reflection). Same package on purpose: R4
// reflects the unexported mirror type taskTypePerformanceResponse.
//
// Fixture provenance: unlike fulfillment-execution, labor-performance's
// /task-types/{taskType}/performance 200 response carries NO example in the
// spec, so task-type-performance.example.json is DERIVED from the pinned
// TaskTypePerformance schema (every required property, the TaskType enum's
// first value, non-null doubles) rather than copied from an example.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

const specCopy = "testdata/labor-performance.openapi.yaml"

func loadPinnedSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(specCopy)
	if err != nil {
		t.Fatalf("cannot load the pinned spec copy: %v", err)
	}
	return doc
}

func fixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/task-type-performance.example.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// R1: the gateway calls GET /task-types/{taskType}/performance (client.go,
// MeanActualSeconds); the pinned spec must still declare that call site, and
// the path parameter's TaskType enum must still cover exactly the three
// uppercase task types taskTypeForPathId maps this context's PathIds onto --
// a rename or a re-cased enum would strand every mapped path.
func TestSpecCoversTheCallSite(t *testing.T) {
	doc := loadPinnedSpec(t)
	path := doc.Paths.Find("/task-types/{taskType}/performance")
	if path == nil || path.Get == nil {
		t.Fatal("the pinned spec no longer declares GET /task-types/{taskType}/performance")
	}
	if path.Get.OperationID != "getTaskTypePerformance" {
		t.Fatalf("operationId = %q, want getTaskTypePerformance", path.Get.OperationID)
	}
	resp := path.Get.Responses.Value("200")
	if resp == nil || resp.Value.Content["application/json"] == nil {
		t.Fatal("the route's 200 response no longer serves application/json")
	}
	if got := resp.Value.Content["application/json"].Schema.Value; got != doc.Components.Schemas["TaskTypePerformance"].Value {
		t.Fatal("the route's 200 schema is no longer TaskTypePerformance")
	}

	taskType := doc.Components.Schemas["TaskType"].Value
	enum := map[string]bool{}
	for _, v := range taskType.Enum {
		enum[v.(string)] = true
	}
	for _, mapped := range []string{"PICK", "PACK", "SLAM"} {
		if !enum[mapped] {
			t.Errorf("the TaskType enum no longer contains %q, which taskTypeForPathId sends", mapped)
		}
	}
}

// R2: the derived wire fixture satisfies the pinned TaskTypePerformance
// schema -- including its nullable doubles.
func TestFixtureSatisfiesThePinnedSchema(t *testing.T) {
	var body any
	if err := json.Unmarshal(fixture(t), &body); err != nil {
		t.Fatal(err)
	}
	schema := loadPinnedSpec(t).Components.Schemas["TaskTypePerformance"].Value
	if err := schema.VisitJSON(body); err != nil {
		t.Fatalf("the derived fixture violates TaskTypePerformance: %v", err)
	}
}

// request records what the gateway actually sent.
type request struct {
	method, path string
}

func servePerformance(t *testing.T, status int, body []byte) (*Client, *request) {
	t.Helper()
	got := &request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, srv.Client()), got
}

// R3: the real client decodes the fixture over a real HTTP round trip and
// returns the measured mean the caller falls back from.
func TestClientDecodesTheFixture(t *testing.T) {
	c, got := servePerformance(t, http.StatusOK, fixture(t))
	seconds, err := c.MeanActualSeconds(t.Context(), shared.PathId("pick"))
	if err != nil {
		t.Fatal(err)
	}
	if seconds != 12.5 {
		t.Fatalf("meanActualSeconds = %v, want 12.5", seconds)
	}
	if got.method != http.MethodGet || got.path != "/task-types/PICK/performance" {
		t.Fatalf("request = %s %s, want GET /task-types/PICK/performance", got.method, got.path)
	}
}

// R3 (contract-shaped failure): the spec documents 400 problem+json for an
// unknown task type and the client folds it -- with a transport failure, a
// 5xx, a garbage body and a null meanActualSeconds (a real 200 with no
// measurable data yet) -- into ports.ErrMeasuredRateUnavailable.
func TestClientMapsFailuresToTheSentinel(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"problem json": {http.StatusBadRequest, `{"type":"https://errors.labor-performance.warehouse-systems.dev/unknown-task-type","title":"Unknown task type","status":400,"detail":"task type X is not known"}`},
		"server error": {http.StatusInternalServerError, `{}`},
		"garbage":      {http.StatusOK, `not json`},
		"null measure": {http.StatusOK, `{"taskType":"PICK","taskCount":0,"meanEfficiencyPct":null,"meanActualSeconds":null}`},
	} {
		c, _ := servePerformance(t, tc.status, []byte(tc.body))
		if _, err := c.MeanActualSeconds(t.Context(), shared.PathId("pick")); !errors.Is(err, ports.ErrMeasuredRateUnavailable) {
			t.Errorf("%s: got %v, want ErrMeasuredRateUnavailable", name, err)
		}
	}
}

// R4: the local mirror DTO and the pinned schema agree in both directions,
// tolerant of producer-side additions the DTO does not (yet) mirror.
func TestResponseDTOMatchesThePinnedSchema(t *testing.T) {
	schema := loadPinnedSpec(t).Components.Schemas["TaskTypePerformance"].Value
	tags := map[string]bool{}
	typ := reflect.TypeOf(taskTypePerformanceResponse{})
	for i := range typ.NumField() {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			tags[tag] = true
		}
	}
	for tag := range tags {
		if _, declared := schema.Properties[tag]; !declared {
			t.Errorf("the DTO mirrors %q, which the pinned spec does not declare", tag)
		}
	}
	for _, required := range schema.Required {
		if _, mirrored := tags[required]; !mirrored {
			t.Errorf("the pinned spec requires %q, which the DTO does not mirror", required)
		}
	}
}
