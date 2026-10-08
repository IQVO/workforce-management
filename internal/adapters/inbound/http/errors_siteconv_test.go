package http

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// The RFC 7807 catalogue stays one-for-one: every status-mapped sentinel has
// exactly one category, slugs are unique per sentinel, and the ADR 0035
// problems are documented in docs/docs/api-reference/errors.md.
func TestProblemCatalog_SiteKeyProblems(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantSlug   string
	}{
		{"conflicting siteCode and buildingId", shared.ErrConflictingSiteAndBuilding, http.StatusUnprocessableEntity, "conflicting-site-and-building"},
		{"missing siteCode/buildingId keeps its slug", shared.ErrMissingSiteKey, http.StatusBadRequest, "missing-building-id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusFor(tc.err); got != tc.wantStatus {
				t.Fatalf("statusFor = %d, want %d", got, tc.wantStatus)
			}
			if got := categoryFor(tc.wantStatus, tc.err).slug; got != tc.wantSlug {
				t.Fatalf("categoryFor slug = %q, want %q", got, tc.wantSlug)
			}
			wrapped := errors.Join(errors.New("ctx"), tc.err)
			if got := categoryFor(tc.wantStatus, wrapped).slug; got != tc.wantSlug {
				t.Fatalf("a wrapped error must keep its slug, got %q", got)
			}
		})
	}
}

func TestProblemCatalog_SlugsAreUniquePerSentinel(t *testing.T) {
	seen := map[error]bool{}
	for _, e := range problemCategoryCatalog {
		if seen[e.sentinel] {
			t.Errorf("sentinel %v appears twice in the catalogue", e.sentinel)
		}
		seen[e.sentinel] = true
	}
}

func TestErrorsDoc_ListsTheSiteKeyProblems(t *testing.T) {
	raw, err := os.ReadFile("../../../../docs/docs/api-reference/errors.md")
	if err != nil {
		t.Fatalf("read errors.md: %v", err)
	}
	doc := string(raw)
	for _, slug := range []string{"/conflicting-site-and-building", "/missing-building-id"} {
		if !strings.Contains(doc, "`"+slug+"`") {
			t.Errorf("docs/docs/api-reference/errors.md does not list `%s`", slug)
		}
	}
	if !strings.Contains(doc, "`422 Unprocessable Content`") && !strings.Contains(doc, "`422 Unprocessable Entity`") {
		t.Errorf("errors.md must list the 422 problem section")
	}
}
