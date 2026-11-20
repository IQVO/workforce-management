package fulfillmentexecution

// Consumer contract tests against the PINNED COPY of fulfillment-execution's
// published spec (testdata/fulfillment-execution.openapi.yaml, pinned from
// IQVO/fulfillment-execution apis/openapi.yaml@develop in contracts/pinned.json
// and kept equal by the pinned-contracts CI job; refresh = copy, never edit).
//
// The archetypes, per the fleet consumer-contract pilot:
//
//	R1 the pinned spec declares the route this gateway calls
//	R2 the wire fixture satisfies the pinned response schema
//	R3 the real gateway decodes the fixture (httptest) and maps failures
//	R4 the response DTO's json tags match the pinned schema (reflection)
//
// Same package (not _test) on purpose: R4 reflects the unexported mirror type
// installedCapacityResponse. kin-openapi is a test-only dependency.

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

const specCopy = "testdata/fulfillment-execution.openapi.yaml"

// loadPinnedSpec resolves the pinned copy with kin-openapi's loader, so $refs
// (responses, schemas) are followed exactly as a consumer tool would.
func loadPinnedSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(specCopy)
	if err != nil {
		t.Fatalf("cannot load the pinned spec copy: %v", err)
	}
	return doc
}

// fixture is the spec's own 200 example for GET /capacity/{capability}
// (capability: pick, installed: 4) -- never hand-tuned, so the test cannot
// drift from what the producer actually documents.
func fixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/installed-capacity.example.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// R1: the gateway calls GET /capacity/{capability} (client.go, InstalledCapacity);
// the pinned spec must still declare exactly that call site, with the path
// parameter it escapes into the URL and the 200 schema the client decodes.
func TestSpecCoversTheCallSite(t *testing.T) {
	doc := loadPinnedSpec(t)
	path := doc.Paths.Find("/capacity/{capability}")
	if path == nil || path.Get == nil {
		t.Fatal("the pinned spec no longer declares GET /capacity/{capability}")
	}
	if path.Get.OperationID != "getInstalledCapacity" {
		t.Fatalf("operationId = %q, want getInstalledCapacity", path.Get.OperationID)
	}
	var param *openapi3.Parameter
	for _, p := range path.Get.Parameters {
		if p.Value != nil && p.Value.Name == "capability" && p.Value.In == "path" {
			param = p.Value
		}
	}
	if param == nil || !param.Required {
		t.Fatal("the route no longer declares the required path parameter 'capability'")
	}
	resp := path.Get.Responses.Value("200")
	if resp == nil || resp.Value.Content["application/json"] == nil {
		t.Fatal("the route's 200 response no longer serves application/json")
	}
	if got := resp.Value.Content["application/json"].Schema.Value; got != doc.Components.Schemas["InstalledCapacityResponse"].Value {
		t.Fatal("the route's 200 schema is no longer InstalledCapacityResponse")
	}
}

// R2: the wire fixture satisfies the pinned InstalledCapacityResponse schema.
func TestFixtureSatisfiesThePinnedSchema(t *testing.T) {
	var body any
	if err := json.Unmarshal(fixture(t), &body); err != nil {
		t.Fatal(err)
	}
	schema := loadPinnedSpec(t).Components.Schemas["InstalledCapacityResponse"].Value
	if err := schema.VisitJSON(body); err != nil {
		t.Fatalf("the spec's own example violates InstalledCapacityResponse: %v", err)
	}
}

// request records what the gateway actually sent.
type request struct {
	method, path string
}

// serveCapacity answers every request with status and body and records the last one.
func serveCapacity(t *testing.T, status int, body []byte) (*Client, *request) {
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

// R3: the real client decodes the spec's example over a real HTTP round trip.
func TestClientDecodesTheSpecExample(t *testing.T) {
	c, got := serveCapacity(t, http.StatusOK, fixture(t))
	installed, err := c.InstalledCapacity(t.Context(), shared.Capability("pick"))
	if err != nil {
		t.Fatal(err)
	}
	if installed != 4 {
		t.Fatalf("installed = %d, want 4 (the spec example's value)", installed)
	}
	if got.method != http.MethodGet || got.path != "/capacity/pick" {
		t.Fatalf("request = %s %s, want GET /capacity/pick", got.method, got.path)
	}
}

// R3 (failure mapping): the client folds ANY failure -- problem+json, a bare
// 5xx, a garbage body -- into ports.ErrInstalledCapacityUnavailable, the one
// sentinel CommitShiftPlan branches on.
func TestClientMapsFailuresToTheSentinel(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"problem json": {http.StatusBadRequest, `{"type":"https://errors.fulfillment-execution.warehouse-systems.dev/unknown-capability","title":"Unknown capability","status":400,"detail":"no such capability"}`},
		"server error": {http.StatusInternalServerError, `{}`},
		"garbage":      {http.StatusOK, `not json`},
	} {
		c, _ := serveCapacity(t, tc.status, []byte(tc.body))
		if _, err := c.InstalledCapacity(t.Context(), shared.Capability("pick")); !errors.Is(err, ports.ErrInstalledCapacityUnavailable) {
			t.Errorf("%s: got %v, want ErrInstalledCapacityUnavailable", name, err)
		}
	}
}

// jsonTags maps the DTO's json field names to presence, so both halves of R4
// can be asserted: every tag is declared, every required field is mirrored.
func jsonTags[T any]() map[string]bool {
	tags := map[string]bool{}
	typ := reflect.TypeOf(*new(T))
	for i := range typ.NumField() {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			tags[tag] = true
		}
	}
	return tags
}

// R4: the local mirror DTO and the pinned schema must agree in both
// directions, with tolerance for producer-side additions: encoding/json
// silently ignores a property the DTO does not mirror (absence-tolerance), so
// the spec may declare MORE than the DTO, never LESS.
func TestResponseDTOMatchesThePinnedSchema(t *testing.T) {
	schema := loadPinnedSpec(t).Components.Schemas["InstalledCapacityResponse"].Value
	tags := jsonTags[installedCapacityResponse]()
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
