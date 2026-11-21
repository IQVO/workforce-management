package http

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"strings"
	"testing"
)

// The fleet replay-class rule (ADR 0044,
// .claude/rules/fleet/idempotency-and-outbox.md): every problem type this
// package can answer declares exactly one replay class on its catalogue
// entry. These unit tests hold the rule without a database; the rendered
// harness template (idempotency_replay_classes_integration_test.go) proves
// the middleware's behaviour per class over a real Postgres.

// classifiedProblemCategories is every declared problemCategory of this
// package with the status it is answered with — the domain catalogue plus
// the middleware/writer-owned problems and the fallbacks.
func classifiedProblemCategories(t *testing.T) []problemCategory {
	t.Helper()
	cats := make([]problemCategory, 0, len(problemCategoryCatalog)+10)
	statuses := make(map[string]int, len(problemCategoryCatalog)+10)
	add := func(status int, cat problemCategory) {
		if prev, dup := statuses[cat.slug]; dup {
			t.Fatalf("problem type %q is declared twice (status %d and %d): one slug, one class", cat.slug, prev, status)
		}
		statuses[cat.slug] = status
		cats = append(cats, cat)
	}
	for _, entry := range problemCategoryCatalog {
		add(statusFor(entry.sentinel), entry.category)
	}
	add(http.StatusBadRequest, idempotencyKeyRequiredProblem)
	add(http.StatusBadRequest, idempotencyKeyInvalidProblem)
	add(http.StatusConflict, idempotencyKeyReusedProblem)
	add(http.StatusBadRequest, malformedRequestBodyProblem)
	add(http.StatusInternalServerError, internalErrorProblem)
	return cats
}

// TestEveryProblemCategoryDeclaresAReplayClass is the unit-level half of the
// rule: an entry without a class (or with a misspelt one) fails here, with
// no database and no runtime default to hide it.
func TestEveryProblemCategoryDeclaresAReplayClass(t *testing.T) {
	cats := classifiedProblemCategories(t)
	if len(cats) < 20 {
		t.Fatalf("only %d problem categories found: the walker is broken, the test would pass on nothing", len(cats))
	}
	for _, cat := range cats {
		switch cat.class {
		case replayFinal, replayOutcome, replayRetryable:
		default:
			t.Errorf("problem type %q declares no replay class (%q): set FINAL, OUTCOME or RETRYABLE on its catalogue entry in errors.go (ADR 0044)", cat.slug, cat.class)
		}
	}
}

// TestFiveHundredClassIsRetryable pins the one class a 5xx problem type may
// declare: a 5xx is never stored (ADR 0027 addendum), so its class is
// RETRYABLE by definition.
func TestFiveHundredClassIsRetryable(t *testing.T) {
	fiveHundreds := map[string]int{
		internalErrorProblem.slug:         http.StatusInternalServerError,
		dependencyUnavailableProblem.slug: http.StatusServiceUnavailable,
	}
	for slug := range fiveHundreds {
		for _, cat := range classifiedProblemCategories(t) {
			if cat.slug == slug && cat.class != replayRetryable {
				t.Errorf("problem type %q answers 5xx: its class must be RETRYABLE, got %q", slug, cat.class)
			}
		}
	}
	// The domain catalogue's own 503 must be RETRYABLE too.
	for _, entry := range problemCategoryCatalog {
		if statusFor(entry.sentinel) == http.StatusServiceUnavailable && entry.category.class != replayRetryable {
			t.Errorf("problem type %q answers 503: its class must be RETRYABLE, got %q", entry.category.slug, entry.category.class)
		}
	}
}

// TestProblemCategoryLiteralsLiveOnlyInErrorsGo fails on any problemCategory
// composite literal outside errors.go: the one declaration point is the one
// classification point, so a future handler cannot add an unclassified
// problem type by writing a literal where no test walks it.
func TestProblemCategoryLiteralsLiveOnlyInErrorsGo(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse this package: %v", err)
	}
	for name, pkg := range pkgs {
		if strings.HasSuffix(name, "_test") {
			continue
		}
		for fname, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if ident, ok := lit.Type.(*ast.Ident); ok && ident.Name == "problemCategory" && !strings.HasSuffix(fname, "errors.go") {
					t.Errorf("%s: a problemCategory literal outside errors.go — declare it (classified) next to the catalogue instead", fset.Position(lit.Pos()))
				}
				return true
			})
		}
	}
}

// TestADRClassTableMatchesTheCatalogue holds the class table of ADR 0044
// equal to the declared catalogue: the document and the code cannot drift.
func TestADRClassTableMatchesTheCatalogue(t *testing.T) {
	raw, err := os.ReadFile("../../../../docs/docs/adr/0044-idempotency-replay-classes.md")
	if err != nil {
		t.Fatalf("read ADR 0044: %v", err)
	}
	adr := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(trimmed, "|"), "|")
		if len(cells) < 3 {
			continue
		}
		slug := strings.TrimSpace(cells[0])
		// only the class-table rows name their problem type in backticks
		if !strings.HasPrefix(slug, "`") {
			continue
		}
		class := strings.TrimSpace(cells[len(cells)-2])
		adr[strings.Trim(slug, "`")] = class
	}
	if len(adr) == 0 {
		t.Fatal("no class rows parsed from ADR 0044: the table walker is broken")
	}
	for _, cat := range classifiedProblemCategories(t) {
		want, ok := adr[cat.slug]
		if !ok {
			t.Errorf("problem type %q has no row in ADR 0044's class table: add it (with its class %q)", cat.slug, cat.class)
			continue
		}
		if want != string(cat.class) {
			t.Errorf("problem type %q: ADR 0044 says %q, the catalogue declares %q", cat.slug, want, cat.class)
		}
	}
	for slug := range adr {
		if slug == "dependency-unavailable" || slug == "unknown-query-parameter" || slug == "invalid-limit" || slug == "invalid-cursor" {
			continue // off-keyed-route problems: documented in the ADR, deliberately not in the idempotent-route walker
		}
		found := false
		for _, cat := range classifiedProblemCategories(t) {
			if cat.slug == slug {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ADR 0044 classifies %q, but no such problem type is declared in errors.go", slug)
		}
	}
}

// TestRefusalClassesCoverTheKeyedRouteSentinels is a sanity net: every
// sentinel of the catalogue must answer a status the middleware can see on a
// keyed route, and no two classes may share a slug with different statuses.
func TestRefusalClassesCoverTheKeyedRouteSentinels(t *testing.T) {
	seen := map[string]int{}
	for _, entry := range problemCategoryCatalog {
		status := statusFor(entry.sentinel)
		if prev, dup := seen[entry.category.slug]; dup && prev != status {
			t.Fatalf("problem type %q answers both %d and %d", entry.category.slug, prev, status)
		}
		seen[entry.category.slug] = status
		if status == 0 {
			t.Errorf("problem type %q maps to status 0", entry.category.slug)
		}
	}
	if len(seen) != len(problemCategoryCatalog) {
		t.Fatalf("catalogue has %d entries but %d distinct slugs: a slug must have exactly one class", len(problemCategoryCatalog), len(seen))
	}
}
