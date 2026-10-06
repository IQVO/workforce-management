package http

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// pathParam normalises "{id}" / "{associateId}" so route and spec templates
// compare equal whatever the parameter is called.
var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// TestRouterRoutesMatchOpenAPISpec is a drift guard: every route the router
// registers must be declared in apis/openapi.yaml with the same method, and
// every operation the spec declares must be served. It exists because
// GET /readyz was served for months but missing from the spec.
func TestRouterRoutesMatchOpenAPISpec(t *testing.T) {
	raw, err := os.ReadFile("../../../../apis/openapi.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	inSpec := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			switch strings.ToUpper(method) {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				inSpec[strings.ToUpper(method)+" "+pathParam.ReplaceAllString(path, "{}")] = true
			}
		}
	}

	routes, ok := NewRouter(newTestHandler(), testLogger, "").(chi.Routes)
	if !ok {
		t.Fatal("router is not a chi.Routes")
	}
	inRouter := map[string]bool{}
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		inRouter[method+" "+pathParam.ReplaceAllString(route, "{}")] = true
		return nil
	}); err != nil {
		t.Fatalf("walk router: %v", err)
	}

	var missingFromSpec, missingFromRouter []string
	for k := range inRouter {
		if !inSpec[k] {
			missingFromSpec = append(missingFromSpec, k)
		}
	}
	for k := range inSpec {
		if !inRouter[k] {
			missingFromRouter = append(missingFromRouter, k)
		}
	}
	sort.Strings(missingFromSpec)
	sort.Strings(missingFromRouter)
	if len(missingFromSpec) > 0 {
		t.Errorf("routes served but not declared in apis/openapi.yaml: %v", missingFromSpec)
	}
	if len(missingFromRouter) > 0 {
		t.Errorf("operations declared in apis/openapi.yaml but not served: %v", missingFromRouter)
	}
}
