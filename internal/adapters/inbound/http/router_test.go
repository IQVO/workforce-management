package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

// testCatalogue mirrors warehouse-infra's real sortable-fc.yaml declared
// paths and fulfillment-execution's/wes-work-planning's own MatchPrefix
// matching semantics — see fulfillment-execution's ADR-0017.
func testCatalogue() *pathcatalog.Catalogue {
	return pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PICK", MatchPrefix: "pick", RequiredCapabilities: []string{"pick"}},
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
		{Id: "REBIN", MatchPrefix: "rebin", RequiredCapabilities: []string{"rebin"}},
		{Id: "SLAM", MatchPrefix: "slam", RequiredCapabilities: []string{"slam"}},
	})
}

// fakeInstalledCapacityClient is a test double for
// ports.InstalledCapacityClient, keyed by station CAPABILITY (what
// fulfillment-execution counts), not by path id. A nil
// capacityByCapability (the zero value)
// means "always return unlimited" -- so tests focused on the
// caller-supplied installedStations check don't need to separately
// script the live-capacity fetch. Tests that DO care about the live
// ceiling set capacityByCapability explicitly.
type fakeInstalledCapacityClient struct {
	capacityByCapability map[shared.Capability]int
	err                  error
}

func (f *fakeInstalledCapacityClient) InstalledCapacity(_ context.Context, capability shared.Capability) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.capacityByCapability == nil {
		return math.MaxInt32, nil
	}
	return f.capacityByCapability[capability], nil
}

func newTestHandler() *Handler {
	associates := memory.NewAssociateRepo()
	shiftPlans := memory.NewShiftPlanRepo()
	assignments := memory.NewAssignmentRepo()
	pub := events.NewLogPublisher(nil)
	clock := &fixedClock{now: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}
	const maxHoursPerShift = 8.0

	return &Handler{
		StartAssociateShift: &usecases.StartAssociateShift{Associates: associates, Events: pub, Clock: clock},
		CertifyAssociate:    &usecases.CertifyAssociate{Associates: associates, Events: pub, Clock: clock},
		ProposePathPlan:     &usecases.ProposePathPlan{Events: pub, Clock: clock},
		CommitShiftPlan:     &usecases.CommitShiftPlan{ShiftPlans: shiftPlans, Events: pub, Clock: clock, InstalledCapacity: &fakeInstalledCapacityClient{}, Catalogue: testCatalogue(), MaxHoursPerShift: maxHoursPerShift},
		AssignLabor:         &usecases.AssignLabor{Associates: associates, Assignments: assignments, Events: pub, Clock: clock, MaxHoursPerShift: maxHoursPerShift},
		StartBreak:          &usecases.StartBreak{Associates: associates, Events: pub, Clock: clock},
		EndBreak:            &usecases.EndBreak{Associates: associates, Events: pub, Clock: clock},
		GetStaffingGap:      &usecases.GetStaffingGap{ShiftPlans: shiftPlans, Assignments: assignments, Events: pub, Clock: clock},
		EndAssociateShift:   &usecases.EndAssociateShift{Associates: associates, Assignments: assignments, Events: pub, Clock: clock, MaxHoursPerShift: maxHoursPerShift},
		Catalogue:           testCatalogue(),
	}
}

func doRequest(t *testing.T, r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestStartShift(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{Certifications: []string{"pack"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/associates/assoc-1" {
		t.Fatalf("expected Location /associates/assoc-1, got %q", loc)
	}
}

// TestStartShift_RejectsEmptyCertification is an HTTP-layer validation test:
// the decoded DTO is validated (via shared.NewCertification) before the use
// case runs, so an empty certification string is rejected as 400 RFC 7807
// rather than reaching the domain/use-case layer.
func TestStartShift_RejectsEmptyCertification(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{Certifications: []string{""}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusBadRequest, "empty-certification", "/associates/assoc-1/start-shift")
}

// TestStartShift_RejectsNullBody pins the wire contract for a literal
// JSON null request body: the spec's requestBody is a required object,
// and every StartShiftRequest field is optional, so without the
// pointer-decode a null body silently created a shift (201). It must be
// a 400 malformed-request-body problem instead.
func TestStartShift_RejectsNullBody(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", json.RawMessage("null"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusBadRequest, "malformed-request-body", "/associates/assoc-1/start-shift")
}

func TestCertify(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{})

	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/certifications", certifyRequest{Certification: "hazmat"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCertify_NotFound(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/associates/ghost/certifications", certifyRequest{Certification: "hazmat"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusNotFound, "resource-not-found", "/associates/ghost/certifications")
}

func TestProposePathPlan(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/paths/pack/plan/propose", proposePathPlanRequest{BuildingId: "bldg-1", Charge: chargePtr(100), PlannedRate: 30})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp proposePathPlanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ProposedHeads != 4 {
		t.Fatalf("expected 4 proposed heads, got %d", resp.ProposedHeads)
	}
	if resp.RateSource != "caller" {
		t.Fatalf("expected rateSource caller, got %q", resp.RateSource)
	}
	if resp.ResolvedRate != 30 {
		t.Fatalf("expected resolvedRate 30, got %v", resp.ResolvedRate)
	}
}

// TestProposePathPlan_RejectsUnknownPathId is the core behavior change the
// process-path catalogue introduces here: a caller-supplied path_id the
// catalogue does not recognize is now rejected outright, rather than
// silently accepted into a ChargeForecast/PathPlan nothing downstream will
// ever route real work through. See fulfillment-execution's ADR-0017 and
// this service's own ADR-0013.
func TestProposePathPlan_RejectsUnknownPathId(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/paths/not-a-real-path/plan/propose", proposePathPlanRequest{BuildingId: "bldg-1", Charge: chargePtr(100), PlannedRate: 30})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusBadRequest, "unknown-path-id", "/paths/not-a-real-path/plan/propose")
}

// TestProposePathPlan_ResolvesRealFleetPathIdVariant proves the catalogue's
// MatchPrefix family matching (not an exact-match lookup) — the exact
// regression class fulfillment-execution's ADR-0017 addendum documents.
func TestProposePathPlan_ResolvesRealFleetPathIdVariant(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/paths/pick-zone-a/plan/propose", proposePathPlanRequest{BuildingId: "bldg-1", Charge: chargePtr(100), PlannedRate: 30})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestProposePathPlan_OmittedRateFallsBackToZeroHeadsWithoutMeasuredRate
// covers the wire-level contract when plannedRate is omitted and no
// MeasuredRateClient is wired (newTestHandler leaves it nil): the request
// must still succeed with 0 proposed heads, never a 4xx/5xx.
func TestProposePathPlan_OmittedRateFallsBackToZeroHeadsWithoutMeasuredRate(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/paths/pack/plan/propose", proposePathPlanRequest{BuildingId: "bldg-1", Charge: chargePtr(100)})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp proposePathPlanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ProposedHeads != 0 {
		t.Fatalf("expected 0 proposed heads with no rate available, got %d", resp.ProposedHeads)
	}
}

// chargePtr builds a *float64 for proposePathPlanRequest literals: Charge
// is a pointer so an omitted field (nil) is distinguishable from an
// explicit 0 charge in tests, mirroring the wire-level distinction the
// handler enforces.
func chargePtr(v float64) *float64 { return &v }

// TestProposePathPlan_MissingChargeReturns400 pins the wire contract for
// an omitted charge: the spec marks charge required, and the handler must
// reject its absence with a 400 missing-charge problem+json rather than
// silently coercing it to a 0 charge (an explicit 0 stays valid — a
// legitimate "nothing to do" proposal).
func TestProposePathPlan_MissingChargeReturns400(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/paths/pack/plan/propose", proposePathPlanRequest{BuildingId: "bldg-1", PlannedRate: 30})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusBadRequest, "missing-charge", "/paths/pack/plan/propose")
}

// TestProposePathPlan_NullChargeReturns400 proves the nil-check also
// covers an explicit JSON null (invalid for a required, non-nullable
// field), not just an omitted key.
func TestProposePathPlan_NullChargeReturns400(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/paths/pack/plan/propose", map[string]any{"buildingId": "bldg-1", "charge": nil, "plannedRate": 30})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusBadRequest, "missing-charge", "/paths/pack/plan/propose")
}

// TestProposePathPlan_ExplicitZeroChargeIsAccepted proves the nil-check
// above does not over-reject: an explicit 0 charge is a valid proposal
// (0 heads) and must stay a 200.
func TestProposePathPlan_ExplicitZeroChargeIsAccepted(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/paths/pack/plan/propose", proposePathPlanRequest{BuildingId: "bldg-1", Charge: chargePtr(0), PlannedRate: 30})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// fakeIdleShareClient is an HTTP-layer test double for ports.IdleShareClient.
type fakeIdleShareClient struct {
	share float64
	err   error
}

func (f *fakeIdleShareClient) IdleSharePct(_ context.Context, _ shared.PathId) (float64, error) {
	return f.share, f.err
}

// TestProposePathPlan_HighIdleShareTrimsProposalOverHTTP is a wire-level
// test for the idleness-as-staffing-signal trim: a high observed idle
// share must be reflected in a lower proposedHeads and a non-empty
// trimReason on the JSON response.
func TestProposePathPlan_HighIdleShareTrimsProposalOverHTTP(t *testing.T) {
	handler := newTestHandler()
	handler.ProposePathPlan = &usecases.ProposePathPlan{
		Events:    handler.ProposePathPlan.Events,
		Clock:     handler.ProposePathPlan.Clock,
		IdleShare: &fakeIdleShareClient{share: 0.5},
	}
	router := NewRouter(handler, testLogger, "")
	rec := doRequest(t, router, http.MethodPost, "/paths/pack/plan/propose", proposePathPlanRequest{BuildingId: "bldg-1", Charge: chargePtr(100), PlannedRate: 10})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp proposePathPlanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// ceil(100/10) = 10 heads before trim; share 0.5 > default 0.30
	// threshold trims to ceil(10*(1-0.5)) = 5.
	if resp.ProposedHeads != 5 {
		t.Fatalf("expected 5 proposed heads after trim, got %d", resp.ProposedHeads)
	}
	if resp.TrimReason == "" {
		t.Fatal("expected a non-empty trimReason when a trim was applied")
	}
}

func TestCommitShiftPlan(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	req := commitShiftPlanRequest{
		BuildingId: "bldg-1",
		ShiftId:    "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40, InstalledStations: 10},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/shift-plans", req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/shift-plans/bldg-1/shift-1" {
		t.Fatalf("expected Location /shift-plans/bldg-1/shift-1, got %q", loc)
	}
}

// TestCommitShiftPlan_RejectsPlannedHeadsExceedingInstalledStations is a
// Definition-of-Done named failing-path test at the HTTP layer.
func TestCommitShiftPlan_RejectsPlannedHeadsExceedingInstalledStations(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	req := commitShiftPlanRequest{
		BuildingId: "bldg-1",
		ShiftId:    "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "pack", PlannedHeads: 11, PlannedRate: 30, PlannedHours: 40, InstalledStations: 10},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/shift-plans", req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusConflict, "planned-heads-exceed-installed", "/shift-plans")
}

// TestCommitShiftPlan_RejectsPlanExceedingLiveInstalledCapacity is a
// Definition-of-Done named failing-path test for Feature C at the HTTP
// layer: even when the caller-supplied installedStations check passes,
// a live fulfillment-execution capacity below plannedHeads is a
// separate 409.
func TestCommitShiftPlan_RejectsPlanExceedingLiveInstalledCapacity(t *testing.T) {
	handler := newTestHandler()
	handler.CommitShiftPlan.InstalledCapacity = &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 3}}
	router := NewRouter(handler, testLogger, "")

	req := commitShiftPlanRequest{
		BuildingId: "bldg-1",
		ShiftId:    "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40, InstalledStations: 10},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/shift-plans", req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusConflict, "exceeds-installed-capacity", "/shift-plans")
}

// TestCommitShiftPlan_CanonicalPathIdIsCheckedAgainstItsCapability is the
// HTTP-level regression for the live bug where POST /shift-plans for the
// canonical path id "PICK" always failed 409 exceeds-installed-capacity:
// the service used to query fulfillment-execution with the PATH id
// ("PICK" -> 0 stations) instead of the path's required CAPABILITY
// ("pick" -> the real count). The fake reports capacity only for the
// lower-case capability, exactly like the real endpoint.
func TestCommitShiftPlan_CanonicalPathIdIsCheckedAgainstItsCapability(t *testing.T) {
	handler := newTestHandler()
	handler.CommitShiftPlan.InstalledCapacity = &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pick": 41}}
	router := NewRouter(handler, testLogger, "")

	req := commitShiftPlanRequest{
		BuildingId: "bldg-1",
		ShiftId:    "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "PICK", PlannedHeads: 4, PlannedRate: 30, PlannedHours: 32, InstalledStations: 10},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/shift-plans", req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCommitShiftPlan_ServiceUnavailableWhenInstalledCapacityUnreachable
// proves the fail-loud policy surfaces as 503, not 500 or 409: a
// dependency-reachability failure is distinct from both a client
// validation error and a genuine bug in this service. See ADR-0014.
func TestCommitShiftPlan_ServiceUnavailableWhenInstalledCapacityUnreachable(t *testing.T) {
	handler := newTestHandler()
	handler.CommitShiftPlan.InstalledCapacity = &fakeInstalledCapacityClient{err: ports.ErrInstalledCapacityUnavailable}
	router := NewRouter(handler, testLogger, "")

	req := commitShiftPlanRequest{
		BuildingId: "bldg-1",
		ShiftId:    "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40, InstalledStations: 10},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/shift-plans", req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusServiceUnavailable, "installed-capacity-unavailable", "/shift-plans")
}

// TestCommitShiftPlan_RejectsMissingBuildingId is an HTTP-layer validation
// test: buildingId/shiftId have no domain value object (ShiftPlan stores
// them as plain strings), so the adapter itself must reject an empty
// buildingId as 400 before calling the use case.
func TestCommitShiftPlan_RejectsMissingBuildingId(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	req := commitShiftPlanRequest{
		ShiftId: "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40, InstalledStations: 10},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/shift-plans", req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusBadRequest, "missing-building-id", "/shift-plans")
}

// TestCommitShiftPlan_RejectsUnknownPathId proves the same catalogue
// validation contract on the multi-line commit path: a single line
// referencing an unrecognized path_id fails the whole request, mirroring
// the fail-loud contract fulfillment-execution's WorkReleased consumer
// (ADR-0017) already has for the same category of gap.
func TestCommitShiftPlan_RejectsUnknownPathId(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	req := commitShiftPlanRequest{
		BuildingId: "bldg-1",
		ShiftId:    "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "not-a-real-path", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40, InstalledStations: 10},
		},
	}
	rec := doRequest(t, router, http.MethodPost, "/shift-plans", req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusBadRequest, "unknown-path-id", "/shift-plans")
}

func TestAssignLabor(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{Certifications: []string{"pack"}})

	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/assignments", assignLaborRequest{PathId: "pack"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/associates/assoc-1/assignments" {
		t.Fatalf("expected Location /associates/assoc-1/assignments, got %q", loc)
	}
}

// TestAssignLabor_RejectsMissingCertification is a Definition-of-Done named
// failing-path test at the HTTP layer.
func TestAssignLabor_RejectsMissingCertification(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{})

	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/assignments", assignLaborRequest{PathId: "pick"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusConflict, "certification-required", "/associates/assoc-1/assignments")
}

// TestAssignLabor_RejectsWhileOnBreak is a Definition-of-Done named
// failing-path test at the HTTP layer.
func TestAssignLabor_RejectsWhileOnBreak(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{Certifications: []string{"pack"}})
	doRequest(t, router, http.MethodPost, "/associates/assoc-1/break/start", nil)

	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/assignments", assignLaborRequest{PathId: "pack"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAssignLabor_RejectsUnknownPathId proves the catalogue validation
// contract on the assignment path too: an unrecognized path_id is
// rejected before ever reaching the AssignLabor use case's own
// certification/break/shift-state gates.
func TestAssignLabor_RejectsUnknownPathId(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{Certifications: []string{"pack"}})

	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/assignments", assignLaborRequest{PathId: "not-a-real-path"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusBadRequest, "unknown-path-id", "/associates/assoc-1/assignments")
}

func TestStartAndEndBreak(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{})

	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/break/start", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, router, http.MethodPost, "/associates/assoc-1/break/end", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestStaffingGap(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	req := commitShiftPlanRequest{
		BuildingId: "bldg-1",
		ShiftId:    "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 24, InstalledStations: 10},
		},
	}
	doRequest(t, router, http.MethodPost, "/shift-plans", req)

	rec := doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp staffingGapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Understaffed || resp.PlannedHeads != 3 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// TestStaffingGap_RejectsUnknownPathId proves the catalogue validation
// contract extends to the read-only staffing-gap lookup too: an
// unrecognized path_id is a genuinely different failure than "no plan
// exists yet for this valid path" (ports.ErrNotFound), so it is
// rejected before the use case ever runs.
func TestStaffingGap_RejectsUnknownPathId(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodGet, "/paths/not-a-real-path/staffing-gap?buildingId=bldg-1&shiftId=shift-1", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusBadRequest, "unknown-path-id", "/paths/not-a-real-path/staffing-gap")
}

// TestStaffingGap_ObservedIdlePctOmittedWhenUnwired covers the default,
// permissive configuration: newTestHandler leaves IdleShare nil, so
// observedIdlePct must be entirely absent from the JSON body (omitempty
// on a nil pointer), never a fabricated 0.
func TestStaffingGap_ObservedIdlePctOmittedWhenUnwired(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	req := commitShiftPlanRequest{
		BuildingId: "bldg-1",
		ShiftId:    "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 24, InstalledStations: 10},
		},
	}
	doRequest(t, router, http.MethodPost, "/shift-plans", req)

	rec := doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "observedIdlePct") {
		t.Fatalf("expected observedIdlePct to be omitted entirely when unwired, got body: %s", rec.Body.String())
	}
}

// TestStaffingGap_ObservedIdlePctSurfacedOverHTTP is a wire-level test
// for the idleness-as-staffing-signal surfacing: a wired IdleShareClient
// reporting a value must appear verbatim as observedIdlePct on the JSON
// response.
func TestStaffingGap_ObservedIdlePctSurfacedOverHTTP(t *testing.T) {
	handler := newTestHandler()
	handler.GetStaffingGap.IdleShare = &fakeIdleShareClient{share: 0.42}
	router := NewRouter(handler, testLogger, "")
	req := commitShiftPlanRequest{
		BuildingId: "bldg-1",
		ShiftId:    "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 24, InstalledStations: 10},
		},
	}
	doRequest(t, router, http.MethodPost, "/shift-plans", req)

	rec := doRequest(t, router, http.MethodGet, "/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp staffingGapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ObservedIdlePct == nil {
		t.Fatal("expected observedIdlePct to be present when IdleShare is wired")
	}
	if *resp.ObservedIdlePct != 0.42 {
		t.Fatalf("observedIdlePct = %v, want 0.42", *resp.ObservedIdlePct)
	}
}

// --- ADR-0011 fast-follow: GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap ---

// TestStaffingGapForShift_ListsEveryPlannedPath is the HTTP-layer proof
// that the fleet-wide list endpoint answers every path in the committed
// plan, matching the single-path lookup's per-path values exactly.
func TestStaffingGapForShift_ListsEveryPlannedPath(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	req := commitShiftPlanRequest{
		BuildingId: "bldg-1",
		ShiftId:    "shift-1",
		Lines: []pathPlanLineRequest{
			{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 24, InstalledStations: 10},
			{PathId: "pick", PlannedHeads: 1, PlannedRate: 25, PlannedHours: 8, InstalledStations: 10},
		},
	}
	doRequest(t, router, http.MethodPost, "/shift-plans", req)

	rec := doRequest(t, router, http.MethodGet, "/buildings/bldg-1/shifts/shift-1/staffing-gap", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp []staffingGapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 entries (one per planned path), got %d: %+v", len(resp), resp)
	}
	byPath := map[string]staffingGapResponse{}
	for _, g := range resp {
		byPath[g.PathId] = g
	}
	if pack, ok := byPath["pack"]; !ok || !pack.Understaffed || pack.PlannedHeads != 3 {
		t.Fatalf("unexpected pack entry: %+v (ok=%v)", pack, ok)
	}
	if pick, ok := byPath["pick"]; !ok || !pick.Understaffed || pick.PlannedHeads != 1 {
		t.Fatalf("unexpected pick entry: %+v (ok=%v)", pick, ok)
	}
}

// TestStaffingGapForShift_UnknownPlan_Returns404 mirrors the single-path
// lookup's not-found behavior for a (buildingId, shiftId) with no
// committed plan.
func TestStaffingGapForShift_UnknownPlan_Returns404(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	rec := doRequest(t, router, http.MethodGet, "/buildings/bldg-1/shifts/shift-1/staffing-gap", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEndShift(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	doRequest(t, router, http.MethodPost, "/associates/assoc-1/start-shift", startShiftRequest{})

	rec := doRequest(t, router, http.MethodPost, "/associates/assoc-1/end-shift", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestMalformedRequestBody is an HTTP-layer validation test covering the
// generic (non-domain-sentinel) RFC 7807 category: malformed JSON falls
// back to the status-keyed "malformed-request-body" category.
func TestMalformedRequestBody(t *testing.T) {
	router := NewRouter(newTestHandler(), testLogger, "")
	req := httptest.NewRequest(http.MethodPost, "/associates/assoc-1/start-shift", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	assertProblemDetails(t, rec, http.StatusBadRequest, "malformed-request-body", "/associates/assoc-1/start-shift")
}

// assertProblemDetails asserts rec's body is a well-formed RFC 7807 problem
// document (Content-Type: application/problem+json) matching status and
// wantSlug (the last path segment of "type"), with instance == wantInstance.
func assertProblemDetails(t *testing.T, rec *httptest.ResponseRecorder, status int, wantSlug, wantInstance string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("expected Content-Type application/problem+json, got %q", ct)
	}
	var pd problemDetails
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
		t.Fatalf("unmarshal problem details: %v", err)
	}
	wantType := problemErrorsURIBase + "/" + wantSlug
	if pd.Type != wantType {
		t.Fatalf("expected type %q, got %q", wantType, pd.Type)
	}
	if pd.Status != status {
		t.Fatalf("expected status %d, got %d", status, pd.Status)
	}
	if pd.Title == "" {
		t.Fatalf("expected non-empty title")
	}
	if pd.Detail == "" {
		t.Fatalf("expected non-empty detail")
	}
	if pd.Instance != wantInstance {
		t.Fatalf("expected instance %q, got %q", wantInstance, pd.Instance)
	}
}
