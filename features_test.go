// Package bdd drives the Workforce Management REST API as a black-box
// Cucumber/Gherkin acceptance suite via godog. The scenarios under
// features/ are executed against the real chi router wired to the same
// in-memory adapters the HTTP adapter's own tests use (memory repos, a
// buffered log publisher, a fixed clock), served over a real
// httptest.Server and driven with real net/http calls — so nothing here
// reaches past the REST boundary into the application or domain layers.
package bdd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"

	inboundhttp "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// maxHoursPerShift mirrors the limit the HTTP adapter's own tests use, so
// ShiftPlan capacity arithmetic in the feature files matches the service.
const maxHoursPerShift = 8.0

// problemTypeBase is the RFC 7807 "type" URI prefix this service emits; the
// feature files name only the trailing category slug.
const problemTypeBase = "https://errors.workforce-management.warehouse-systems.dev/"

// fixedClock pins time so scenarios are deterministic.
type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

// unlimitedInstalledCapacity is a test double for
// ports.InstalledCapacityClient that always reports capacity as
// effectively unlimited. The BDD scenarios under features/ exercise the
// caller-supplied installedStations invariant (this context's own
// long-standing check); the LIVE fulfillment-execution-sourced ceiling
// Feature C added is covered at the application layer instead (see
// internal/application/usecases/usecases_test.go), so this suite treats
// it as always-satisfied rather than re-deriving fulfillment-execution
// fixtures here.
type unlimitedInstalledCapacity struct{}

func (unlimitedInstalledCapacity) InstalledCapacity(_ context.Context, _ shared.PathId) (int, error) {
	return math.MaxInt32, nil
}

// newServer builds a fully wired composition of the service backed by
// in-memory adapters and serves it over HTTP. Each scenario gets its own,
// so no state leaks between scenarios. idleShare wires
// usecases.ProposePathPlan's optional idle-share trim signal (nil is the
// default, permissive configuration every other scenario exercises).
func newServer(idleShare ports.IdleShareClient) *httptest.Server {
	associates := memory.NewAssociateRepo()
	shiftPlans := memory.NewShiftPlanRepo()
	assignments := memory.NewAssignmentRepo()
	pub := events.NewLogPublisher(slog.New(slog.NewTextHandler(io.Discard, nil)))
	clock := &fixedClock{now: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}

	handler := &inboundhttp.Handler{
		StartAssociateShift: &usecases.StartAssociateShift{Associates: associates, Events: pub, Clock: clock},
		CertifyAssociate:    &usecases.CertifyAssociate{Associates: associates, Events: pub, Clock: clock},
		ProposePathPlan:     &usecases.ProposePathPlan{Events: pub, Clock: clock, IdleShare: idleShare},
		CommitShiftPlan:     &usecases.CommitShiftPlan{ShiftPlans: shiftPlans, Events: pub, Clock: clock, InstalledCapacity: unlimitedInstalledCapacity{}, MaxHoursPerShift: maxHoursPerShift},
		AssignLabor:         &usecases.AssignLabor{Associates: associates, Assignments: assignments, Events: pub, Clock: clock, MaxHoursPerShift: maxHoursPerShift},
		StartBreak:          &usecases.StartBreak{Associates: associates, Events: pub, Clock: clock},
		EndBreak:            &usecases.EndBreak{Associates: associates, Events: pub, Clock: clock},
		GetStaffingGap:      &usecases.GetStaffingGap{ShiftPlans: shiftPlans, Assignments: assignments, Events: pub, Clock: clock, IdleShare: idleShare},
		EndAssociateShift:   &usecases.EndAssociateShift{Associates: associates, Assignments: assignments, Events: pub, Clock: clock, MaxHoursPerShift: maxHoursPerShift},
	}

	return httptest.NewServer(inboundhttp.NewRouter(handler, slog.New(slog.NewTextHandler(io.Discard, nil)), ""))
}

// fixedIdleShareClient is a BDD-suite test double for
// ports.IdleShareClient: it reports the same share for every path,
// which is all the "high idle share trims the proposal" scenario needs.
type fixedIdleShareClient struct{ share float64 }

func (f fixedIdleShareClient) IdleSharePct(_ context.Context, _ shared.PathId) (float64, error) {
	return f.share, nil
}

// world is the per-scenario state: the server under test plus the most
// recent HTTP response, which the Then steps assert against.
type world struct {
	server *httptest.Server
	client *http.Client

	lastStatus      int
	lastContentType string
	lastBody        []byte
}

func (w *world) reset() {
	w.stop()
	w.server = newServer(nil)
	w.client = w.server.Client()
	w.lastStatus = 0
	w.lastContentType = ""
	w.lastBody = nil
}

func (w *world) stop() {
	if w.server != nil {
		w.server.Close()
		w.server = nil
	}
}

// do issues a real HTTP request against the running server and records the
// response for later assertions.
func (w *world) do(ctx context.Context, method, path string, body any) error {
	var reader io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, w.server.URL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	w.lastStatus = resp.StatusCode
	w.lastContentType = resp.Header.Get("Content-Type")
	w.lastBody = raw
	return nil
}

// expectStatus fails the step when the last response carried a different
// status, quoting the body so failures are diagnosable.
func (w *world) expectStatus(want int) error {
	if w.lastStatus != want {
		return fmt.Errorf("expected status %d, got %d: %s", want, w.lastStatus, string(w.lastBody))
	}
	return nil
}

func (w *world) decodeLast(target any) error {
	if err := json.Unmarshal(w.lastBody, target); err != nil {
		return fmt.Errorf("decode response %q: %w", string(w.lastBody), err)
	}
	return nil
}

// problem is the RFC 7807 error shape every failing endpoint returns.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// expectProblem asserts the last response is a well-formed problem+json of
// the named category with the named status.
func (w *world) expectProblem(status int, slug string) error {
	if err := w.expectStatus(status); err != nil {
		return err
	}
	if !strings.HasPrefix(w.lastContentType, "application/problem+json") {
		return fmt.Errorf("expected application/problem+json, got %q", w.lastContentType)
	}

	var p problem
	if err := w.decodeLast(&p); err != nil {
		return err
	}
	if want := problemTypeBase + slug; p.Type != want {
		return fmt.Errorf("expected problem type %q, got %q (detail: %s)", want, p.Type, p.Detail)
	}
	if p.Status != status {
		return fmt.Errorf("expected problem status field %d, got %d", status, p.Status)
	}
	return nil
}

// ---------------------------------------------------------------------------
// request payload shapes (the BDD suite speaks the published JSON contract,
// it never imports the adapter's unexported DTOs)
// ---------------------------------------------------------------------------

type pathPlanLine struct {
	PathId            string  `json:"pathId"`
	PlannedHeads      int     `json:"plannedHeads"`
	PlannedRate       float64 `json:"plannedRate"`
	PlannedHours      float64 `json:"plannedHours"`
	InstalledStations int     `json:"installedStations"`
}

type shiftPlanBody struct {
	BuildingId string         `json:"buildingId"`
	ShiftId    string         `json:"shiftId"`
	Lines      []pathPlanLine `json:"lines"`
}

type shiftPlanResult struct {
	BuildingId string `json:"buildingId"`
	ShiftId    string `json:"shiftId"`
	Lines      []struct {
		PathId       string  `json:"pathId"`
		PlannedHeads int     `json:"plannedHeads"`
		PlannedRate  float64 `json:"plannedRate"`
		PlannedHours float64 `json:"plannedHours"`
	} `json:"lines"`
}

type assignmentResult struct {
	AssociateId  string `json:"associateId"`
	ActivePathId string `json:"activePathId"`
	Active       bool   `json:"active"`
}

type staffingGapResult struct {
	PathId       string `json:"pathId"`
	PlannedHeads int    `json:"plannedHeads"`
	ActiveHeads  int    `json:"activeHeads"`
	Understaffed bool   `json:"understaffed"`
}

// linesFromTable turns a Gherkin data table of PathPlan lines into the
// request payload. The header row names the columns.
func linesFromTable(table *godog.Table) ([]pathPlanLine, error) {
	if table == nil || len(table.Rows) < 2 {
		return nil, fmt.Errorf("expected a data table with a header row and at least one path plan line")
	}

	header := make(map[string]int, len(table.Rows[0].Cells))
	for i, cell := range table.Rows[0].Cells {
		header[strings.TrimSpace(cell.Value)] = i
	}

	lines := make([]pathPlanLine, 0, len(table.Rows)-1)
	for _, row := range table.Rows[1:] {
		cell := func(name string) string {
			idx, ok := header[name]
			if !ok || idx >= len(row.Cells) {
				return ""
			}
			return strings.TrimSpace(row.Cells[idx].Value)
		}

		var line pathPlanLine
		line.PathId = cell("pathId")
		if _, err := fmt.Sscanf(cell("plannedHeads"), "%d", &line.PlannedHeads); err != nil {
			return nil, fmt.Errorf("plannedHeads %q: %w", cell("plannedHeads"), err)
		}
		if _, err := fmt.Sscanf(cell("plannedRate"), "%g", &line.PlannedRate); err != nil {
			return nil, fmt.Errorf("plannedRate %q: %w", cell("plannedRate"), err)
		}
		if _, err := fmt.Sscanf(cell("plannedHours"), "%g", &line.PlannedHours); err != nil {
			return nil, fmt.Errorf("plannedHours %q: %w", cell("plannedHours"), err)
		}
		if _, err := fmt.Sscanf(cell("installedStations"), "%d", &line.InstalledStations); err != nil {
			return nil, fmt.Errorf("installedStations %q: %w", cell("installedStations"), err)
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// ---------------------------------------------------------------------------
// step implementations — each one is a real REST call
// ---------------------------------------------------------------------------

func (w *world) associateShiftStartedWithCertifications(ctx context.Context, associateId, certs string) error {
	list := []string{}
	for _, c := range strings.Split(certs, ",") {
		if trimmed := strings.TrimSpace(c); trimmed != "" {
			list = append(list, trimmed)
		}
	}
	if err := w.do(ctx, http.MethodPost, "/associates/"+associateId+"/start-shift", map[string]any{"certifications": list}); err != nil {
		return err
	}
	return w.expectStatus(http.StatusCreated)
}

func (w *world) associateShiftStartedWithoutCertifications(ctx context.Context, associateId string) error {
	return w.associateShiftStartedWithCertifications(ctx, associateId, "")
}

func (w *world) associateIsCertifiedFor(ctx context.Context, associateId, certification string) error {
	if err := w.do(ctx, http.MethodPost, "/associates/"+associateId+"/certifications", map[string]any{"certification": certification}); err != nil {
		return err
	}
	return w.expectStatus(http.StatusNoContent)
}

func (w *world) shiftPlanIsCommitted(ctx context.Context, buildingId, shiftId string, table *godog.Table) error {
	if err := w.commitShiftPlan(ctx, buildingId, shiftId, table); err != nil {
		return err
	}
	return w.expectStatus(http.StatusCreated)
}

func (w *world) commitShiftPlan(ctx context.Context, buildingId, shiftId string, table *godog.Table) error {
	lines, err := linesFromTable(table)
	if err != nil {
		return err
	}
	return w.do(ctx, http.MethodPost, "/shift-plans", shiftPlanBody{BuildingId: buildingId, ShiftId: shiftId, Lines: lines})
}

func (w *world) associateIsAssignedToPath(ctx context.Context, associateId, pathId string) error {
	return w.do(ctx, http.MethodPost, "/associates/"+associateId+"/assignments", map[string]any{"pathId": pathId})
}

func (w *world) associateHasStartedABreak(ctx context.Context, associateId string) error {
	if err := w.associateStartsABreak(ctx, associateId); err != nil {
		return err
	}
	return w.expectStatus(http.StatusNoContent)
}

func (w *world) associateStartsABreak(ctx context.Context, associateId string) error {
	return w.do(ctx, http.MethodPost, "/associates/"+associateId+"/break/start", nil)
}

func (w *world) associateEndsTheBreak(ctx context.Context, associateId string) error {
	return w.do(ctx, http.MethodPost, "/associates/"+associateId+"/break/end", nil)
}

func (w *world) staffingGapIsRequested(ctx context.Context, pathId, buildingId, shiftId string) error {
	return w.do(ctx, http.MethodGet, fmt.Sprintf("/paths/%s/staffing-gap?buildingId=%s&shiftId=%s", pathId, buildingId, shiftId), nil)
}

func (w *world) associateEndsTheirShift(ctx context.Context, associateId string) error {
	return w.do(ctx, http.MethodPost, "/associates/"+associateId+"/end-shift", nil)
}

func (w *world) associateHasEndedTheirShift(ctx context.Context, associateId string) error {
	if err := w.associateEndsTheirShift(ctx, associateId); err != nil {
		return err
	}
	return w.expectStatus(http.StatusNoContent)
}

func (w *world) shiftPlanCommitSucceeded(plannedHeads int, pathId string) error {
	if err := w.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var sp shiftPlanResult
	if err := w.decodeLast(&sp); err != nil {
		return err
	}
	for _, line := range sp.Lines {
		if line.PathId != pathId {
			continue
		}
		if line.PlannedHeads != plannedHeads {
			return fmt.Errorf("expected %d planned heads on path %q, got %d", plannedHeads, pathId, line.PlannedHeads)
		}
		return nil
	}
	return fmt.Errorf("committed ShiftPlan has no line for path %q", pathId)
}

func (w *world) laborAssignmentCreatedWithActivePath(pathId string) error {
	if err := w.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var la assignmentResult
	if err := w.decodeLast(&la); err != nil {
		return err
	}
	if !la.Active {
		return fmt.Errorf("expected an ACTIVE LaborAssignment, got active=false")
	}
	if la.ActivePathId != pathId {
		return fmt.Errorf("expected active path %q, got %q", pathId, la.ActivePathId)
	}
	return nil
}

func (w *world) laborAssignmentHasExactlyOneActiveAssignment(associateId, pathId string) error {
	if err := w.laborAssignmentCreatedWithActivePath(pathId); err != nil {
		return err
	}
	var la assignmentResult
	if err := w.decodeLast(&la); err != nil {
		return err
	}
	if la.AssociateId != associateId {
		return fmt.Errorf("expected the LaborAssignment of associate %q, got %q", associateId, la.AssociateId)
	}
	return nil
}

// pathHasActiveHeads reads the staffing-gap projection to count how many
// associates are currently ACTIVE on a path — this is how the suite proves
// the prior assignment was closed rather than double-booked.
func (w *world) pathHasActiveHeads(ctx context.Context, pathId string, activeHeads int, buildingId, shiftId string) error {
	if err := w.staffingGapIsRequested(ctx, pathId, buildingId, shiftId); err != nil {
		return err
	}
	if err := w.expectStatus(http.StatusOK); err != nil {
		return err
	}
	var gap staffingGapResult
	if err := w.decodeLast(&gap); err != nil {
		return err
	}
	if gap.ActiveHeads != activeHeads {
		return fmt.Errorf("expected %d active heads on path %q, got %d", activeHeads, pathId, gap.ActiveHeads)
	}
	return nil
}

func (w *world) pathIsFlaggedUnderstaffed(pathId string, plannedHeads, activeHeads int) error {
	if err := w.expectStatus(http.StatusOK); err != nil {
		return err
	}
	var gap staffingGapResult
	if err := w.decodeLast(&gap); err != nil {
		return err
	}
	if gap.PathId != pathId {
		return fmt.Errorf("expected the staffing gap of path %q, got %q", pathId, gap.PathId)
	}
	if gap.PlannedHeads != plannedHeads {
		return fmt.Errorf("expected %d planned heads, got %d", plannedHeads, gap.PlannedHeads)
	}
	if gap.ActiveHeads != activeHeads {
		return fmt.Errorf("expected %d active heads, got %d", activeHeads, gap.ActiveHeads)
	}
	if !gap.Understaffed {
		return fmt.Errorf("expected path %q to be flagged PathUnderstaffed, got understaffed=false", pathId)
	}
	return nil
}

// --- idleness-as-staffing-signal steps --------------------------------------

// observedIdleShareForPath rebuilds the server wired with a
// fixedIdleShareClient reporting share for every path -- this scenario
// needs the idle-share signal wired BEFORE the plan proposal request, so
// it replaces w.reset()'s default nil wiring rather than mutating the
// already-running server's use case in place.
func (w *world) observedIdleShareForPath(_, shareStr string) error {
	var share float64
	if _, err := fmt.Sscanf(shareStr, "%g", &share); err != nil {
		return fmt.Errorf("idle share %q: %w", shareStr, err)
	}
	w.stop()
	w.server = newServer(fixedIdleShareClient{share: share})
	w.client = w.server.Client()
	return nil
}

type proposePathPlanBody struct {
	BuildingId  string  `json:"buildingId"`
	Charge      float64 `json:"charge"`
	PlannedRate float64 `json:"plannedRate"`
}

type proposePathPlanResult struct {
	PathId        string  `json:"pathId"`
	ProposedHeads int     `json:"proposedHeads"`
	ResolvedRate  float64 `json:"resolvedRate"`
	RateSource    string  `json:"rateSource"`
	TrimReason    string  `json:"trimReason"`
}

func (w *world) pathPlanIsProposed(ctx context.Context, pathId, buildingId string, charge, plannedRate float64) error {
	return w.do(ctx, http.MethodPost, "/paths/"+pathId+"/plan/propose", proposePathPlanBody{
		BuildingId:  buildingId,
		Charge:      charge,
		PlannedRate: plannedRate,
	})
}

func (w *world) proposedHeadsAreTrimmedWithNonEmptyTrimReason(wantHeads int) error {
	if err := w.expectStatus(http.StatusOK); err != nil {
		return err
	}
	var resp proposePathPlanResult
	if err := w.decodeLast(&resp); err != nil {
		return err
	}
	if resp.ProposedHeads != wantHeads {
		return fmt.Errorf("expected %d proposed heads after trim, got %d", wantHeads, resp.ProposedHeads)
	}
	if resp.TrimReason == "" {
		return fmt.Errorf("expected a non-empty trimReason when a trim was applied")
	}
	return nil
}

// pathIsNotFlaggedUnderstaffed is the complement of
// pathIsFlaggedUnderstaffed: active assignments meet the plan, so the
// read model must NOT flag the path nor raise PathUnderstaffed.
func (w *world) pathIsNotFlaggedUnderstaffed(pathId string, plannedHeads, activeHeads int) error {
	if err := w.expectStatus(http.StatusOK); err != nil {
		return err
	}
	var gap staffingGapResult
	if err := w.decodeLast(&gap); err != nil {
		return err
	}
	if gap.PathId != pathId {
		return fmt.Errorf("expected the staffing gap of path %q, got %q", pathId, gap.PathId)
	}
	if gap.PlannedHeads != plannedHeads {
		return fmt.Errorf("expected %d planned heads, got %d", plannedHeads, gap.PlannedHeads)
	}
	if gap.ActiveHeads != activeHeads {
		return fmt.Errorf("expected %d active heads, got %d", activeHeads, gap.ActiveHeads)
	}
	if gap.Understaffed {
		return fmt.Errorf("expected path %q NOT to be flagged PathUnderstaffed, got understaffed=true", pathId)
	}
	return nil
}

// proposalSuggestsHeads asserts the propose response's heads, resolved
// rate and rate source — the pure-computation contract of
// POST /paths/{pathId}/plan/propose.
func (w *world) proposalSuggestsHeads(heads int, resolvedRate float64, rateSource string) error {
	if err := w.expectStatus(http.StatusOK); err != nil {
		return err
	}
	var resp proposePathPlanResult
	if err := w.decodeLast(&resp); err != nil {
		return err
	}
	if resp.ProposedHeads != heads {
		return fmt.Errorf("expected %d proposed heads, got %d", heads, resp.ProposedHeads)
	}
	if resp.ResolvedRate != resolvedRate {
		return fmt.Errorf("expected resolved rate %g, got %g", resolvedRate, resp.ResolvedRate)
	}
	if resp.RateSource != rateSource {
		return fmt.Errorf("expected rate source %q, got %q", rateSource, resp.RateSource)
	}
	return nil
}

// InitializeScenario registers the hooks and step definitions for every
// scenario. A fresh server (and therefore fresh in-memory repositories) is
// built before each scenario so scenarios stay independent.
func InitializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		w.reset()
		return ctx, nil
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		w.stop()
		return ctx, nil
	})

	// Given
	sc.Step(`^an AssociateShift is started for associate "([^"]*)" with certifications "([^"]*)"$`, w.associateShiftStartedWithCertifications)
	sc.Step(`^an AssociateShift is started for associate "([^"]*)" with no certifications$`, w.associateShiftStartedWithoutCertifications)
	sc.Step(`^associate "([^"]*)" is certified for "([^"]*)"$`, w.associateIsCertifiedFor)
	sc.Step(`^a ShiftPlan is committed for building "([^"]*)" shift "([^"]*)" with lines:$`, w.shiftPlanIsCommitted)
	sc.Step(`^associate "([^"]*)" has started a break$`, w.associateHasStartedABreak)
	sc.Step(`^associate "([^"]*)" has ended their shift$`, w.associateHasEndedTheirShift)
	sc.Step(`^the observed idle share for path "([^"]*)" is ([\d.]+)$`, w.observedIdleShareForPath)

	// When
	sc.Step(`^committing a ShiftPlan for building "([^"]*)" shift "([^"]*)" with lines:$`, w.commitShiftPlan)
	sc.Step(`^associate "([^"]*)" is assigned to path "([^"]*)"$`, w.associateIsAssignedToPath)
	sc.Step(`^associate "([^"]*)" starts a break$`, w.associateStartsABreak)
	sc.Step(`^associate "([^"]*)" ends the break$`, w.associateEndsTheBreak)
	sc.Step(`^associate "([^"]*)" ends their shift$`, w.associateEndsTheirShift)
	sc.Step(`^the staffing gap for path "([^"]*)" is requested for building "([^"]*)" shift "([^"]*)"$`, w.staffingGapIsRequested)
	sc.Step(`^a path plan is proposed for path "([^"]*)" building "([^"]*)" with charge (\d+) and planned rate (\d+)$`, w.pathPlanIsProposed)

	// Then
	sc.Step(`^the ShiftPlan commit succeeds with (\d+) planned heads on path "([^"]*)"$`, w.shiftPlanCommitSucceeded)
	sc.Step(`^the ShiftPlan commit is rejected with status (\d+) and problem type "([^"]*)"$`, w.expectProblem)
	sc.Step(`^the assignment is rejected with status (\d+) and problem type "([^"]*)"$`, w.expectProblem)
	sc.Step(`^the request is rejected with status (\d+) and problem type "([^"]*)"$`, w.expectProblem)
	sc.Step(`^the last request succeeds with status (\d+)$`, w.expectStatus)
	sc.Step(`^the LaborAssignment is created with active path "([^"]*)"$`, w.laborAssignmentCreatedWithActivePath)
	sc.Step(`^the LaborAssignment for associate "([^"]*)" has exactly one ACTIVE assignment, on path "([^"]*)"$`, w.laborAssignmentHasExactlyOneActiveAssignment)
	sc.Step(`^path "([^"]*)" has (\d+) active heads? in building "([^"]*)" shift "([^"]*)"$`, w.pathHasActiveHeads)
	sc.Step(`^path "([^"]*)" is flagged PathUnderstaffed with (\d+) planned heads and (\d+) active heads?$`, w.pathIsFlaggedUnderstaffed)
	sc.Step(`^path "([^"]*)" reports (\d+) planned heads and (\d+) active heads and is not understaffed$`, w.pathIsNotFlaggedUnderstaffed)
	sc.Step(`^the proposal suggests (\d+) heads at resolved rate (\d+) from source "([^"]*)"$`, w.proposalSuggestsHeads)
	sc.Step(`^the proposed heads are trimmed to (\d+) with a non-empty trim reason$`, w.proposedHeadsAreTrimmedWithNonEmptyTrimReason)
}

// TestFeatures runs every Gherkin feature under features/ as a Go test.
func TestFeatures(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
