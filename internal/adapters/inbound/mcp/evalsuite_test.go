// E3 — behavioral eval suite: Gherkin scenarios in
// testdata/features/mcp_tools.feature driven through godog, the same
// Cucumber-for-Go engine the repo-root REST acceptance suite uses. The
// suite lives inside the mcp package so the evals ship with the adapter
// they evaluate and run in the existing CI test job (`go test ./...`) with
// zero new infrastructure.
package mcp_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// currentEvalT carries the running *testing.T into the scenario world;
// godog's ScenarioInitializer API does not hand it to step contexts, and
// the shared harness needs it for t.Cleanup. Safe here because the suite
// runs scenarios sequentially inside one test function.
var currentEvalT *testing.T

// TestMCPEvalSuite runs every Gherkin scenario under testdata/features
// against a freshly wired MCP server + client session.
func TestMCPEvalSuite(t *testing.T) {
	currentEvalT = t
	suite := godog.TestSuite{
		ScenarioInitializer: initializeMCPEvalScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"testdata/features"},
			Strict:   true,
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run MCP eval scenarios")
	}
}

// mcpEvalWorld is the per-scenario state: one harness (server + session +
// seeded repos) per scenario, rebuilt by the Background step.
type mcpEvalWorld struct {
	h *evalHarness
}

func initializeMCPEvalScenario(sc *godog.ScenarioContext) {
	w := &mcpEvalWorld{}

	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		return ctx, nil
	})

	// Given
	sc.Step(`^the MCP server is running with the canonical eval state \(B1/S1 pack plan of 3 heads; a1 pack, a2 pick, a3 pack\+pick, a4 pack on break; one labor-report row\)$`, w.serverRunning)

	// When
	sc.Step(`^I call the tool "([^"]*)" with argument "([^"]*)" = "([^"]*)"$`, w.callWithStringArg)
	sc.Step(`^I call the tool "([^"]*)" with argument "([^"]*)" = (\d+)$`, w.callWithNumberArg)
	sc.Step(`^I call the tool "([^"]*)" with arguments$`, w.callWithTableArgs)

	// Then
	sc.Step(`^the tool call succeeds$`, w.callSucceeded)
	sc.Step(`^the tool call does not succeed silently$`, w.callDidNotSucceedSilently)
	sc.Step(`^the tool call reports a problem mentioning "([^"]*)"$`, w.callErroredMentioning)
	sc.Step(`^the structured result field "([^"]*)" is "([^"]*)"$`, w.fieldIsString)
	sc.Step(`^the structured result field "([^"]*)" is (\d+)$`, w.fieldIsNumber)
	sc.Step(`^the structured result field "([^"]*)" is true$`, w.fieldIsTrue)
	sc.Step(`^the structured result field "([^"]*)" is false$`, w.fieldIsFalse)
	sc.Step(`^the structured result field "([^"]*)" has (\d+) entries$`, w.fieldHasEntries)
	sc.Step(`^the structured result row (\d+) field "([^"]*)" is "([^"]*)"$`, w.rowFieldIsString)
	sc.Step(`^the structured result row (\d+) field "([^"]*)" is (\d+)$`, w.rowFieldIsNumber)
	sc.Step(`^the domain event "([^"]*)" was published$`, w.eventPublished)
}

func (w *mcpEvalWorld) serverRunning() error {
	// The harness binds its own lifecycle to the running *testing.T via
	// newEvalHarness; the world only carries the pointer.
	w.h = newEvalHarness(currentEvalT)
	return nil
}

func (w *mcpEvalWorld) callWithStringArg(tool, arg, value string) error {
	return w.h.callTool(context.Background(), tool, map[string]any{arg: value})
}

func (w *mcpEvalWorld) callWithNumberArg(tool, arg string, value int64) error {
	return w.h.callTool(context.Background(), tool, map[string]any{arg: value})
}

func (w *mcpEvalWorld) callWithTableArgs(tool string, table *godog.Table) error {
	args := map[string]any{}
	for _, row := range table.Rows {
		cells := row.Cells
		if len(cells) != 2 {
			return fmt.Errorf("arguments table needs exactly two columns, got %d", len(cells))
		}
		key, raw := cells[0].Value, cells[1].Value
		if n, err := strconv.Atoi(raw); err == nil {
			args[key] = n
			continue
		}
		args[key] = raw
	}
	return w.h.callTool(context.Background(), tool, args)
}

func (w *mcpEvalWorld) callSucceeded() error {
	if w.h.lastCallErr != nil {
		return fmt.Errorf("tool call failed: %w", w.h.lastCallErr)
	}
	if w.h.lastCallResult == nil || w.h.lastCallResult.IsError {
		return fmt.Errorf("tool call returned an error result: %s", w.h.lastCallContent)
	}
	return nil
}

func (w *mcpEvalWorld) callDidNotSucceedSilently() error {
	if w.h.lastCallErr != nil {
		return nil // protocol-level rejection
	}
	if w.h.lastCallResult != nil && w.h.lastCallResult.IsError {
		return nil // tool-level rejection
	}
	return fmt.Errorf("the call succeeded silently — wrong-typed arguments must not be coerced")
}

func (w *mcpEvalWorld) callErroredMentioning(fragment string) error {
	if w.h.lastCallErr == nil && (w.h.lastCallResult == nil || !w.h.lastCallResult.IsError) {
		return fmt.Errorf("expected a tool error, got success: %s", w.h.lastCallContent)
	}
	if !containsFold(w.h.lastCallContent, fragment) && w.h.lastCallErr != nil && !containsFold(w.h.lastCallErr.Error(), fragment) {
		return fmt.Errorf("expected the tool error to mention %q, got %q / %v", fragment, w.h.lastCallContent, w.h.lastCallErr)
	}
	return nil
}

func (w *mcpEvalWorld) fieldIsString(field, want string) error {
	got, err := w.structuredField(field)
	if err != nil {
		return err
	}
	if s, ok := got.(string); ok && s == want {
		return nil
	}
	return fmt.Errorf("structured result field %q = %v, want %q", field, got, want)
}

func (w *mcpEvalWorld) fieldIsNumber(field string, want int64) error {
	got, err := w.structuredField(field)
	if err != nil {
		return err
	}
	switch v := got.(type) {
	case float64:
		if int64(v) == want {
			return nil
		}
	case int:
		if int64(v) == want {
			return nil
		}
	case int64:
		if v == want {
			return nil
		}
	}
	return fmt.Errorf("structured result field %q = %v, want %d", field, got, want)
}

func (w *mcpEvalWorld) fieldIsTrue(field string) error {
	got, err := w.structuredField(field)
	if err != nil {
		return err
	}
	if b, ok := got.(bool); ok && b {
		return nil
	}
	return fmt.Errorf("structured result field %q = %v, want true", field, got)
}

func (w *mcpEvalWorld) fieldIsFalse(field string) error {
	got, err := w.structuredField(field)
	if err != nil {
		return err
	}
	if b, ok := got.(bool); ok && !b {
		return nil
	}
	return fmt.Errorf("structured result field %q = %v, want false", field, got)
}

// fieldHasEntries pins the length of an array-valued structured field
// (e.g. the labor report's rows).
func (w *mcpEvalWorld) fieldHasEntries(field string, want int) error {
	got, err := w.structuredField(field)
	if err != nil {
		return err
	}
	arr, ok := got.([]any)
	if !ok {
		return fmt.Errorf("structured result field %q is not an array: %v", field, got)
	}
	if len(arr) != want {
		return fmt.Errorf("structured result field %q has %d entries, want %d", field, len(arr), want)
	}
	return nil
}

// rowFieldIsString pins a string field of one row of an array-valued
// structured field (e.g. rows[0].pathId of the labor report).
func (w *mcpEvalWorld) rowFieldIsString(row int, field, want string) error {
	got, err := w.rowField(row, field)
	if err != nil {
		return err
	}
	if s, ok := got.(string); ok && s == want {
		return nil
	}
	return fmt.Errorf("structured result row %d field %q = %v, want %q", row, field, got, want)
}

// rowFieldIsNumber pins a numeric field of one row of an array-valued
// structured field (e.g. rows[0].laborAssigned of the labor report).
func (w *mcpEvalWorld) rowFieldIsNumber(row int, field string, want int64) error {
	got, err := w.rowField(row, field)
	if err != nil {
		return err
	}
	if v, ok := got.(float64); ok && int64(v) == want {
		return nil
	}
	return fmt.Errorf("structured result row %d field %q = %v, want %d", row, field, got, want)
}

func (w *mcpEvalWorld) rowField(row int, field string) (any, error) {
	if w.h.lastCallResult == nil {
		return nil, fmt.Errorf("no tool result recorded")
	}
	obj, ok := w.h.lastCallResult.StructuredContent.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("structured result is not an object: %+v", w.h.lastCallResult.StructuredContent)
	}
	rows, ok := obj["rows"].([]any)
	if !ok || row < 0 || row >= len(rows) {
		return nil, fmt.Errorf("structured result has no rows[%d]: %+v", row, obj)
	}
	entry, ok := rows[row].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("structured result rows[%d] is not an object: %+v", row, rows[row])
	}
	got, ok := entry[field]
	if !ok {
		return nil, fmt.Errorf("structured result rows[%d] has no field %q: %+v", row, field, entry)
	}
	return got, nil
}

func (w *mcpEvalWorld) structuredField(field string) (any, error) {
	if w.h.lastCallResult == nil {
		return nil, fmt.Errorf("no tool result recorded")
	}
	obj, ok := w.h.lastCallResult.StructuredContent.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("structured result is not an object: %+v", w.h.lastCallResult.StructuredContent)
	}
	got, ok := obj[field]
	if !ok {
		return nil, fmt.Errorf("structured result has no field %q: %+v", field, obj)
	}
	return got, nil
}

func (w *mcpEvalWorld) eventPublished(name string) error {
	for _, e := range w.h.publisher.Events() {
		if e.EventName() == name {
			return nil
		}
	}
	return fmt.Errorf("expected domain event %q to be published", name)
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
