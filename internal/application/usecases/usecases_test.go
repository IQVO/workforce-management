package usecases

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/memory"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/assignment"
	"github.com/claudioed/workforce-management/internal/domain/associate"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// fixedClock lets tests advance time deterministically.
type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

type fixtures struct {
	associates  *memory.AssociateRepo
	shiftPlans  *memory.ShiftPlanRepo
	assignments *memory.AssignmentRepo
	pub         *events.LogPublisher
	clock       *fixedClock
}

func newFixtures() *fixtures {
	return &fixtures{
		associates:  memory.NewAssociateRepo(),
		shiftPlans:  memory.NewShiftPlanRepo(),
		assignments: memory.NewAssignmentRepo(),
		pub:         events.NewLogPublisher(slog.New(slog.NewTextHandler(io.Discard, nil))),
		clock:       &fixedClock{now: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)},
	}
}

// errBoom is a sentinel error injected by the failing* fakes below to
// exercise use-case error-handling branches the in-memory adapters can
// never trigger on their own: they never return an error from Save, and
// only ever return ports.ErrNotFound from a lookup.

var errBoom = errors.New("boom")

// failingAssociateRepo wraps a real *memory.AssociateRepo so tests can force
// Save or FindByID to fail while otherwise delegating to the real repo.
type failingAssociateRepo struct {
	*memory.AssociateRepo
	saveErr error
	findErr error
}

func (r *failingAssociateRepo) Save(ctx context.Context, a *associate.AssociateShift) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	return r.AssociateRepo.Save(ctx, a)
}

func (r *failingAssociateRepo) FindByID(ctx context.Context, id shared.AssociateId) (*associate.AssociateShift, error) {
	if r.findErr != nil {
		return nil, r.findErr
	}
	return r.AssociateRepo.FindByID(ctx, id)
}

// failingShiftPlanRepo wraps a real *memory.ShiftPlanRepo so tests can force
// Save or FindByBuildingAndShift to fail while otherwise delegating to the
// real repo.
type failingShiftPlanRepo struct {
	*memory.ShiftPlanRepo
	saveErr error
	findErr error
}

func (r *failingShiftPlanRepo) Save(ctx context.Context, sp *shiftplan.ShiftPlan) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	return r.ShiftPlanRepo.Save(ctx, sp)
}

func (r *failingShiftPlanRepo) FindByBuildingAndShift(ctx context.Context, buildingId, shiftId string) (*shiftplan.ShiftPlan, error) {
	if r.findErr != nil {
		return nil, r.findErr
	}
	return r.ShiftPlanRepo.FindByBuildingAndShift(ctx, buildingId, shiftId)
}

// failingAssignmentRepo wraps a real *memory.AssignmentRepo so tests can
// force Save, FindByAssociateID, or CountActiveByPath to fail while
// otherwise delegating to the real repo.
type failingAssignmentRepo struct {
	*memory.AssignmentRepo
	saveErr  error
	findErr  error
	countErr error
}

func (r *failingAssignmentRepo) Save(ctx context.Context, la *assignment.LaborAssignment) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	return r.AssignmentRepo.Save(ctx, la)
}

func (r *failingAssignmentRepo) FindByAssociateID(ctx context.Context, id shared.AssociateId) (*assignment.LaborAssignment, error) {
	if r.findErr != nil {
		return nil, r.findErr
	}
	return r.AssignmentRepo.FindByAssociateID(ctx, id)
}

func (r *failingAssignmentRepo) CountActiveByPath(ctx context.Context, pathId shared.PathId) (int, error) {
	if r.countErr != nil {
		return 0, r.countErr
	}
	return r.AssignmentRepo.CountActiveByPath(ctx, pathId)
}

// failingPublisher wraps a real *events.LogPublisher so tests can force
// Publish to fail while otherwise delegating to the real publisher.
type failingPublisher struct {
	*events.LogPublisher
	err error
}

func (p *failingPublisher) Publish(ctx context.Context, evts ...shared.DomainEvent) error {
	if p.err != nil {
		return p.err
	}
	return p.LogPublisher.Publish(ctx, evts...)
}

func TestStartAssociateShift_PersistsAndPublishes(t *testing.T) {
	f := newFixtures()
	uc := &StartAssociateShift{Associates: f.associates, Events: f.pub, Clock: f.clock}

	shift, err := uc.Execute(context.Background(), "assoc-1", []shared.Certification{"pack"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shift.AssociateId() != "assoc-1" {
		t.Fatalf("unexpected associate id: %v", shift.AssociateId())
	}

	stored, err := f.associates.FindByID(context.Background(), "assoc-1")
	if err != nil {
		t.Fatalf("expected associate to be persisted: %v", err)
	}
	if !stored.HasCertification("pack") {
		t.Fatal("expected persisted associate to hold certification")
	}

	found := false
	for _, e := range f.pub.Events() {
		if e.EventName() == "AssociateShiftStarted" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected AssociateShiftStarted to be published")
	}
}

func TestCertifyAssociate_AddsCertification(t *testing.T) {
	f := newFixtures()
	start := &StartAssociateShift{Associates: f.associates, Events: f.pub, Clock: f.clock}
	if _, err := start.Execute(context.Background(), "assoc-1", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	certify := &CertifyAssociate{Associates: f.associates, Events: f.pub, Clock: f.clock}
	if err := certify.Execute(context.Background(), "assoc-1", "hazmat"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stored, _ := f.associates.FindByID(context.Background(), "assoc-1")
	if !stored.HasCertification("hazmat") {
		t.Fatal("expected certification to persist")
	}
}

func TestCertifyAssociate_NotFound(t *testing.T) {
	f := newFixtures()
	certify := &CertifyAssociate{Associates: f.associates, Events: f.pub, Clock: f.clock}
	err := certify.Execute(context.Background(), "ghost", "hazmat")
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestProposePathPlan_ComputesCeil(t *testing.T) {
	f := newFixtures()
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock}
	heads, resolvedRate, rateSource, _, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 30)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if heads != 4 {
		t.Fatalf("expected 4 heads, got %d", heads)
	}
	if resolvedRate != 30 {
		t.Fatalf("expected resolvedRate 30, got %v", resolvedRate)
	}
	if rateSource != RateSourceCaller {
		t.Fatalf("expected rateSource %q, got %q", RateSourceCaller, rateSource)
	}
}

// fakeMeasuredRateClient is a test double for ports.MeasuredRateClient.
type fakeMeasuredRateClient struct {
	seconds float64
	err     error
	called  bool
}

func (f *fakeMeasuredRateClient) MeanActualSeconds(_ context.Context, _ shared.PathId) (float64, error) {
	f.called = true
	return f.seconds, f.err
}

// fakeIdleShareClient is a test double for ports.IdleShareClient.
type fakeIdleShareClient struct {
	share  float64
	err    error
	called bool
}

func (f *fakeIdleShareClient) IdleSharePct(_ context.Context, _ shared.PathId) (float64, error) {
	f.called = true
	return f.share, f.err
}

// fakeInstalledCapacityClient is a test double for
// ports.InstalledCapacityClient. capacityByCapability scripts a
// per-CAPABILITY return value (fulfillment-execution counts stations by
// capability, not by path); err (if set) applies to every call, taking
// precedence over capacityByCapability -- mirroring
// fakeMeasuredRateClient's shape. calledFor records every capability
// actually queried, so a test can prove which one hit the wire.
type fakeInstalledCapacityClient struct {
	capacityByCapability map[shared.Capability]int
	err                  error
	calledFor            []shared.Capability
}

func (f *fakeInstalledCapacityClient) InstalledCapacity(_ context.Context, capability shared.Capability) (int, error) {
	f.calledFor = append(f.calledFor, capability)
	if f.err != nil {
		return 0, f.err
	}
	return f.capacityByCapability[capability], nil
}

// testCatalogue mirrors the fleet's real process-path catalogue
// (warehouse-infra's sortable-fc.yaml / process-path-management): the
// canonical path ids are UPPER-case, while the capability each requires
// is the lower-case string stations are registered with in
// fulfillment-execution.
func testCatalogue() *pathcatalog.Catalogue {
	return pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PICK", MatchPrefix: "pick", RequiredCapabilities: []string{"pick"}},
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
		{Id: "REBIN", MatchPrefix: "rebin", RequiredCapabilities: []string{"rebin"}},
		{Id: "SLAM", MatchPrefix: "slam", RequiredCapabilities: []string{"slam"}},
	})
}

// TestProposePathPlan_FallsBackToMeasuredRateWhenNoCallerRate covers the
// core close-the-loop behaviour: an omitted (<=0) plannedRate consults
// MeasuredRate and uses it when available.
func TestProposePathPlan_FallsBackToMeasuredRateWhenNoCallerRate(t *testing.T) {
	f := newFixtures()
	measured := &fakeMeasuredRateClient{seconds: 25}
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, MeasuredRate: measured}

	heads, resolvedRate, rateSource, _, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !measured.called {
		t.Fatal("expected MeasuredRate to be consulted when plannedRate is not supplied")
	}
	if resolvedRate != 25 {
		t.Fatalf("expected resolvedRate 25, got %v", resolvedRate)
	}
	if rateSource != RateSourceMeasured {
		t.Fatalf("expected rateSource %q, got %q", RateSourceMeasured, rateSource)
	}
	if heads != 4 { // ceil(100/25)
		t.Fatalf("expected 4 heads, got %d", heads)
	}
}

// TestProposePathPlan_CallerRateAlwaysWinsOverMeasured covers the other
// half of the contract: a caller-supplied rate is never overridden by a
// measured one, and MeasuredRate is not even consulted.
func TestProposePathPlan_CallerRateAlwaysWinsOverMeasured(t *testing.T) {
	f := newFixtures()
	measured := &fakeMeasuredRateClient{seconds: 999}
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, MeasuredRate: measured}

	heads, resolvedRate, rateSource, _, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 30)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if measured.called {
		t.Fatal("expected MeasuredRate NOT to be consulted when a caller rate is supplied")
	}
	if resolvedRate != 30 || rateSource != RateSourceCaller {
		t.Fatalf("expected caller rate 30 to win, got resolvedRate=%v rateSource=%q", resolvedRate, rateSource)
	}
	if heads != 4 {
		t.Fatalf("expected 4 heads, got %d", heads)
	}
}

// TestProposePathPlan_MeasuredRateUnavailableFallsBackToZeroHeads covers
// the fail-quiet contract: ErrMeasuredRateUnavailable (service down, no
// TaskType mapping, no data yet) must never fail the request -- it falls
// back to the existing zero-rate behaviour (0 heads), same as today with
// no plannedRate at all.
func TestProposePathPlan_MeasuredRateUnavailableFallsBackToZeroHeads(t *testing.T) {
	f := newFixtures()
	measured := &fakeMeasuredRateClient{err: ports.ErrMeasuredRateUnavailable}
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, MeasuredRate: measured}

	heads, resolvedRate, rateSource, _, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 0)
	if err != nil {
		t.Fatalf("expected no error (fail-quiet), got %v", err)
	}
	if heads != 0 {
		t.Fatalf("expected 0 heads when no rate is available at all, got %d", heads)
	}
	if resolvedRate != 0 || rateSource != RateSourceCaller {
		t.Fatalf("expected resolvedRate=0 rateSource=caller on fallback, got resolvedRate=%v rateSource=%q", resolvedRate, rateSource)
	}
}

// TestProposePathPlan_NilMeasuredRateClientBehavesAsBeforeTheFeature
// covers the composition-root safety net: a nil MeasuredRate (e.g. an
// older wiring, or a test that doesn't care) must not panic, and must
// behave exactly like the pre-feature zero-rate case.
func TestProposePathPlan_NilMeasuredRateClientBehavesAsBeforeTheFeature(t *testing.T) {
	f := newFixtures()
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock} // MeasuredRate left nil
	heads, resolvedRate, rateSource, _, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if heads != 0 || resolvedRate != 0 || rateSource != RateSourceCaller {
		t.Fatalf("expected zero-rate fallback, got heads=%d resolvedRate=%v rateSource=%q", heads, resolvedRate, rateSource)
	}
}

// TestProposePathPlan_MeasuredRateClientNonSentinelErrorPropagates
// covers the "programming error, not a business condition" branch: any
// error OTHER than ErrMeasuredRateUnavailable from a MeasuredRateClient
// must propagate as a hard failure, not be silently swallowed.
func TestProposePathPlan_MeasuredRateClientNonSentinelErrorPropagates(t *testing.T) {
	f := newFixtures()
	measured := &fakeMeasuredRateClient{err: errBoom}
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, MeasuredRate: measured}

	_, _, _, _, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 0)
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom to propagate, got %v", err)
	}
}

// --- idleness-as-staffing-signal: ProposePathPlan idle-share trim -----------

// TestProposePathPlan_IdleShareTrim covers the boundary cases named in
// the plan: exactly at threshold (no trim -- share must EXCEED the
// threshold, not merely meet it), just above (trim applied), just below
// (no trim), nil/no-data fail-open (no trim), and floor-at-1-head
// clamping when a trim would otherwise go below 1.
func TestProposePathPlan_IdleShareTrim(t *testing.T) {
	const threshold = 0.30

	tests := []struct {
		name        string
		heads       int // via charge/rate: charge=heads*10, rate=10
		idleShare   float64
		idleErr     error
		nilIdle     bool
		wantHeads   int
		wantTrimmed bool
	}{
		{
			name:      "exactly at threshold is NOT trimmed (share must exceed, not meet)",
			heads:     10,
			idleShare: threshold,
			wantHeads: 10,
		},
		{
			name:        "just above threshold is trimmed",
			heads:       10,
			idleShare:   threshold + 0.01,
			wantHeads:   int(math.Ceil(10 * (1 - (threshold + 0.01)))),
			wantTrimmed: true,
		},
		{
			name:      "just below threshold is NOT trimmed",
			heads:     10,
			idleShare: threshold - 0.01,
			wantHeads: 10,
		},
		{
			name:      "nil/no-data fails open: no trim",
			heads:     10,
			nilIdle:   true,
			wantHeads: 10,
		},
		{
			name:        "floor at 1 head minimum when trim would go below 1",
			heads:       2,
			idleShare:   0.95,
			wantHeads:   1,
			wantTrimmed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtures()
			var idleClient ports.IdleShareClient
			if !tc.nilIdle {
				idleClient = &fakeIdleShareClient{share: tc.idleShare, err: tc.idleErr}
			}
			uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, IdleShare: idleClient}

			charge := float64(tc.heads) * 10
			heads, _, _, trimReason, err := uc.Execute(context.Background(), "bldg-1", "pack", charge, 10)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if heads != tc.wantHeads {
				t.Fatalf("heads = %d, want %d", heads, tc.wantHeads)
			}
			if tc.wantTrimmed && trimReason == "" {
				t.Fatal("expected a non-empty trimReason when a trim was applied")
			}
			if !tc.wantTrimmed && trimReason != "" {
				t.Fatalf("expected no trim, got trimReason %q", trimReason)
			}
		})
	}
}

// TestProposePathPlan_IdleShareUnavailableFailsOpen covers the
// ErrIdleShareUnavailable fail-open path explicitly (no TaskType
// mapping, or genuinely no idle-share data yet) -- must never trim and
// must never fail Execute.
func TestProposePathPlan_IdleShareUnavailableFailsOpen(t *testing.T) {
	f := newFixtures()
	idleClient := &fakeIdleShareClient{err: ports.ErrIdleShareUnavailable}
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, IdleShare: idleClient}

	heads, _, _, trimReason, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !idleClient.called {
		t.Fatal("expected IdleShare to be consulted")
	}
	if heads != 10 {
		t.Fatalf("expected no trim (10 heads), got %d", heads)
	}
	if trimReason != "" {
		t.Fatalf("expected empty trimReason on fail-open, got %q", trimReason)
	}
}

// TestProposePathPlan_NilIdleShareClientAppliesNoTrim covers the
// composition-root safety net: a nil IdleShare (the default -- no
// LABOR_PERFORMANCE_MODE=kafka-cache wired) must not panic and must
// never trim, matching every other *_MODE permissive-by-default pattern.
func TestProposePathPlan_NilIdleShareClientAppliesNoTrim(t *testing.T) {
	f := newFixtures()
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock} // IdleShare left nil

	heads, _, _, trimReason, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if heads != 10 || trimReason != "" {
		t.Fatalf("expected no trim (10 heads, empty reason), got heads=%d trimReason=%q", heads, trimReason)
	}
}

// TestProposePathPlan_IdleShareClientNonSentinelErrorPropagates mirrors
// the MeasuredRateClient contract: any error other than
// ErrIdleShareUnavailable from an IdleShareClient is a programming error
// and must propagate as a hard failure.
func TestProposePathPlan_IdleShareClientNonSentinelErrorPropagates(t *testing.T) {
	f := newFixtures()
	idleClient := &fakeIdleShareClient{err: errBoom}
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, IdleShare: idleClient}

	_, _, _, _, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 10)
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom to propagate, got %v", err)
	}
}

// TestProposePathPlan_ZeroHeadsNeverConsultsIdleShare: when the resolved
// rate yields 0 proposed heads already, there is nothing to trim -- the
// idle-share check must be skipped rather than trimming 0 to some other
// value.
func TestProposePathPlan_ZeroHeadsNeverConsultsIdleShare(t *testing.T) {
	f := newFixtures()
	idleClient := &fakeIdleShareClient{share: 0.99}
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, IdleShare: idleClient}

	heads, _, _, trimReason, err := uc.Execute(context.Background(), "bldg-1", "pack", 0, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if heads != 0 || trimReason != "" {
		t.Fatalf("expected 0 heads and no trim, got heads=%d trimReason=%q", heads, trimReason)
	}
	if idleClient.called {
		t.Fatal("expected IdleShare NOT to be consulted when heads is already 0")
	}
}

// TestProposePathPlan_CustomIdleShareTrimThreshold covers the
// IDLE_SHARE_TRIM_THRESHOLD override: a caller-configured threshold
// (rather than DefaultIdleShareTrimThreshold) governs the trim decision.
func TestProposePathPlan_CustomIdleShareTrimThreshold(t *testing.T) {
	f := newFixtures()
	// A share of 0.6 would trim under the default 0.30 threshold, but
	// must NOT trim under an overridden 0.90 threshold.
	idleClient := &fakeIdleShareClient{share: 0.6}
	uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, IdleShare: idleClient, IdleShareTrimThreshold: 0.90}

	heads, _, _, trimReason, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if heads != 10 || trimReason != "" {
		t.Fatalf("expected no trim under the overridden 0.90 threshold, got heads=%d trimReason=%q", heads, trimReason)
	}
}

func TestCommitShiftPlan_PersistsAndPublishes(t *testing.T) {
	f := newFixtures()
	installedCapacity := &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 10}}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: installedCapacity, Catalogue: testCatalogue(), MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40}}
	installed := map[shared.PathId]int{"pack": 10}

	sp, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, installed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sp.PlannedHeadsFor("pack") != 5 {
		t.Fatalf("expected 5 heads, got %d", sp.PlannedHeadsFor("pack"))
	}

	stored, err := f.shiftPlans.FindByBuildingAndShift(context.Background(), "bldg-1", "shift-1")
	if err != nil {
		t.Fatalf("expected plan to be persisted: %v", err)
	}
	if stored.PlannedHeadsFor("pack") != 5 {
		t.Fatalf("expected persisted plan to have 5 heads, got %d", stored.PlannedHeadsFor("pack"))
	}
	if len(installedCapacity.calledFor) != 1 || installedCapacity.calledFor[0] != "pack" {
		t.Fatalf("expected InstalledCapacity to be called once for pack, got %v", installedCapacity.calledFor)
	}
}

// TestCommitShiftPlan_RejectsPlannedHeadsExceedingInstalledStations is a
// Definition-of-Done named failing-path test at the application layer.
func TestCommitShiftPlan_RejectsPlannedHeadsExceedingInstalledStations(t *testing.T) {
	f := newFixtures()
	installedCapacity := &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 10}}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: installedCapacity, Catalogue: testCatalogue(), MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 11, PlannedRate: 30, PlannedHours: 40}}
	installed := map[shared.PathId]int{"pack": 10}

	_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, installed)
	if !errors.Is(err, shiftplan.ErrPlannedHeadsExceedInstalled) {
		t.Fatalf("expected ErrPlannedHeadsExceedInstalled, got %v", err)
	}
}

// TestCommitShiftPlan_RejectsPlanExceedingLiveInstalledCapacity proves
// Feature C at the application layer: even when the caller-supplied
// installedStations check passes, a live fulfillment-execution capacity
// below plannedHeads still rejects the commit.
func TestCommitShiftPlan_RejectsPlanExceedingLiveInstalledCapacity(t *testing.T) {
	f := newFixtures()
	installedCapacity := &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5}}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: installedCapacity, Catalogue: testCatalogue(), MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 8, PlannedRate: 30, PlannedHours: 64}}
	installedStations := map[shared.PathId]int{"pack": 20} // structural check alone would pass

	_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, installedStations)
	if !errors.Is(err, shiftplan.ErrExceedsInstalledCapacity) {
		t.Fatalf("expected ErrExceedsInstalledCapacity, got %v", err)
	}
}

// TestCommitShiftPlan_FailsLoudWhenInstalledCapacityUnavailable is the
// core Feature C policy test: unlike ProposePathPlan's MeasuredRateClient
// (which fails OPEN, falling back to a caller-supplied rate),
// CommitShiftPlan fails the ENTIRE commit when
// ports.ErrInstalledCapacityUnavailable is returned -- a shift-plan
// commit mutates real state, and this fleet's own rule is to fail loud
// for anything that mutates real state.
func TestCommitShiftPlan_FailsLoudWhenInstalledCapacityUnavailable(t *testing.T) {
	f := newFixtures()
	installedCapacity := &fakeInstalledCapacityClient{err: ports.ErrInstalledCapacityUnavailable}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: installedCapacity, Catalogue: testCatalogue(), MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40}}
	installed := map[shared.PathId]int{"pack": 10}

	_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, installed)
	if !errors.Is(err, ports.ErrInstalledCapacityUnavailable) {
		t.Fatalf("expected ErrInstalledCapacityUnavailable, got %v", err)
	}

	// The commit must not have persisted anything -- a failed capacity
	// fetch aborts before the plan is ever constructed.
	if _, findErr := f.shiftPlans.FindByBuildingAndShift(context.Background(), "bldg-1", "shift-1"); !errors.Is(findErr, ports.ErrNotFound) {
		t.Fatalf("expected no plan to be persisted, FindByBuildingAndShift returned: %v", findErr)
	}
}

// TestCommitShiftPlan_FetchesInstalledCapacityOncePerDistinctPath proves
// the dedup behavior: a request with multiple lines for the SAME path
// (a malformed but not domain-invalid request -- the domain layer has no
// opinion about duplicate path lines) must not call InstalledCapacity
// more than once per distinct PathId.
func TestCommitShiftPlan_FetchesInstalledCapacityOncePerDistinctPath(t *testing.T) {
	f := newFixtures()
	installedCapacity := &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 20, "pick": 20}}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: installedCapacity, Catalogue: testCatalogue(), MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 2, PlannedRate: 30, PlannedHours: 16},
		{PathId: "pick", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 24},
	}
	installed := map[shared.PathId]int{"pack": 20, "pick": 20}

	if _, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, installed); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(installedCapacity.calledFor) != 2 {
		t.Fatalf("expected exactly 2 InstalledCapacity calls (one per distinct path), got %v", installedCapacity.calledFor)
	}
}

// TestCommitShiftPlan_QueriesInstalledCapacityByRequiredCapability is the
// regression suite for the live bug where every canonical path id failed
// 409 exceeds-installed-capacity: CommitShiftPlan used to send the PATH id
// to fulfillment-execution's GET /capacity/{capability}, which counts
// stations by the exact lower-case capability they were registered with
// ("pick"), so "PICK" always counted 0. The fake registry below behaves
// exactly like the real endpoint: only the capability string counts.
func TestCommitShiftPlan_QueriesInstalledCapacityByRequiredCapability(t *testing.T) {
	multiCapability := pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PICK", MatchPrefix: "pick", RequiredCapabilities: []string{"pick"}},
		{Id: "HAZMAT-PICK", MatchPrefix: "hazmat-pick", RequiredCapabilities: []string{"pick", "hazmat"}},
		{Id: "GHOST", MatchPrefix: "ghost", RequiredCapabilities: nil},
	})
	registry := map[shared.Capability]int{"pick": 41, "pack": 3, "hazmat": 2}

	tests := []struct {
		name          string
		catalogue     ports.PathCatalogue
		pathId        shared.PathId
		plannedHeads  int
		wantErr       error
		wantQueried   []shared.Capability
		wantCommitted bool
	}{
		{
			name:         "canonical PICK is checked against capability pick, not the path id",
			pathId:       "PICK",
			plannedHeads: 4, wantQueried: []shared.Capability{"pick"}, wantCommitted: true,
		},
		{
			name:         "lower-case path id resolves to the same capability",
			pathId:       "pick",
			plannedHeads: 41, wantQueried: []shared.Capability{"pick"}, wantCommitted: true,
		},
		{
			name:         "suffixed real-fleet path id resolves through the catalogue prefix",
			pathId:       "pick-zone-a",
			plannedHeads: 4, wantQueried: []shared.Capability{"pick"}, wantCommitted: true,
		},
		{
			name:         "plannedHeads above the capability's station count is rejected",
			pathId:       "PACK",
			plannedHeads: 4, wantErr: shiftplan.ErrExceedsInstalledCapacity, wantQueried: []shared.Capability{"pack"},
		},
		{
			name:         "a path requiring several capabilities is capped by the scarcest one",
			catalogue:    multiCapability,
			pathId:       "hazmat-pick",
			plannedHeads: 3, wantErr: shiftplan.ErrExceedsInstalledCapacity, wantQueried: []shared.Capability{"pick", "hazmat"},
		},
		{
			name:         "a multi-capability path within the scarcest count commits",
			catalogue:    multiCapability,
			pathId:       "HAZMAT-PICK",
			plannedHeads: 2, wantQueried: []shared.Capability{"pick", "hazmat"}, wantCommitted: true,
		},
		{
			name:         "a path declaring no capabilities fails closed with a 0 ceiling",
			catalogue:    multiCapability,
			pathId:       "ghost",
			plannedHeads: 1, wantErr: shiftplan.ErrExceedsInstalledCapacity,
		},
		{
			name:         "an unknown path is rejected before fulfillment-execution is called",
			pathId:       "not-a-real-path",
			plannedHeads: 1, wantErr: pathcatalog.ErrUnknownPath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixtures()
			catalogue := tt.catalogue
			if catalogue == nil {
				catalogue = testCatalogue()
			}
			capacity := &fakeInstalledCapacityClient{capacityByCapability: registry}
			uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: capacity, Catalogue: catalogue, MaxHoursPerShift: 8}

			lines := []shiftplan.PathPlan{{PathId: tt.pathId, PlannedHeads: tt.plannedHeads, PlannedRate: 30, PlannedHours: float64(tt.plannedHeads) * 8}}
			_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{tt.pathId: 100})

			assertCommitOutcome(t, err, tt.wantErr)
			assertQueriedCapabilities(t, capacity.calledFor, tt.wantQueried)
			assertPlanPersisted(t, f, tt.wantCommitted)
		})
	}
}

func assertCommitOutcome(t *testing.T, err, wantErr error) {
	t.Helper()
	if wantErr == nil && err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wantErr != nil && !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
}

func assertQueriedCapabilities(t *testing.T, got, want []shared.Capability) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("queried capabilities %v, want %v", got, want)
	}
	for i, c := range want {
		if got[i] != c {
			t.Fatalf("queried capabilities %v, want %v", got, want)
		}
	}
}

func assertPlanPersisted(t *testing.T, f *fixtures, wantCommitted bool) {
	t.Helper()
	_, findErr := f.shiftPlans.FindByBuildingAndShift(context.Background(), "bldg-1", "shift-1")
	if wantCommitted && findErr != nil {
		t.Fatalf("expected the plan to be persisted: %v", findErr)
	}
	if !wantCommitted && !errors.Is(findErr, ports.ErrNotFound) {
		t.Fatalf("expected nothing persisted, FindByBuildingAndShift returned: %v", findErr)
	}
}

// TestCommitShiftPlan_PathIdIsNeverSentAsCapability pins the exact live
// symptom: with the real registry shape ({"pick": 41}), querying by the
// path id "PICK" would see 0 and reject 4 heads. The commit must succeed.
func TestCommitShiftPlan_PathIdIsNeverSentAsCapability(t *testing.T) {
	f := newFixtures()
	capacity := &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pick": 41}}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: capacity, Catalogue: testCatalogue(), MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{{PathId: "PICK", PlannedHeads: 4, PlannedRate: 30, PlannedHours: 32}}
	if _, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"PICK": 10}); err != nil {
		t.Fatalf("expected PICK with 41 pick stations to commit, got %v", err)
	}
	for _, c := range capacity.calledFor {
		if c == "PICK" {
			t.Fatal("the path id PICK was sent to fulfillment-execution as a capability")
		}
	}
}

// TestCommitShiftPlan_UnreachableFulfillmentExecutionStillFailsLoud keeps
// ADR-0014's fail-loud contract intact after the capability resolution
// step was added: the sentinel must reach the caller unwrapped-compatible
// (errors.Is) so the HTTP adapter still answers 503.
func TestCommitShiftPlan_UnreachableFulfillmentExecutionStillFailsLoud(t *testing.T) {
	f := newFixtures()
	capacity := &fakeInstalledCapacityClient{err: ports.ErrInstalledCapacityUnavailable}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: capacity, Catalogue: testCatalogue(), MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{{PathId: "PICK", PlannedHeads: 1, PlannedRate: 30, PlannedHours: 8}}
	_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"PICK": 10})
	if !errors.Is(err, ports.ErrInstalledCapacityUnavailable) {
		t.Fatalf("expected ErrInstalledCapacityUnavailable, got %v", err)
	}
	if len(capacity.calledFor) != 1 || capacity.calledFor[0] != "pick" {
		t.Fatalf("expected one query for capability pick, got %v", capacity.calledFor)
	}
}

// TestCommitShiftPlan_FetchesEachCapabilityOnce proves two different
// paths resolving to the same capability ("pick" and "pick-zone-a") cost
// one fulfillment-execution round trip, not two, and are both bounded by
// the same station count.
func TestCommitShiftPlan_FetchesEachCapabilityOnce(t *testing.T) {
	f := newFixtures()
	capacity := &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pick": 5}}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: capacity, Catalogue: testCatalogue(), MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{
		{PathId: "pick", PlannedHeads: 2, PlannedRate: 30, PlannedHours: 16},
		{PathId: "pick-zone-a", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 24},
	}
	if _, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pick": 10, "pick-zone-a": 10}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(capacity.calledFor) != 1 || capacity.calledFor[0] != "pick" {
		t.Fatalf("expected exactly one query for capability pick, got %v", capacity.calledFor)
	}
}

// TestCommitShiftPlan_RequiresCatalogue proves a mis-wired use case fails
// every commit instead of silently guessing a capability from the path id.
func TestCommitShiftPlan_RequiresCatalogue(t *testing.T) {
	f := newFixtures()
	capacity := &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pick": 41}}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: capacity, MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{{PathId: "pick", PlannedHeads: 1, PlannedRate: 30, PlannedHours: 8}}
	_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pick": 10})
	if !errors.Is(err, ErrCommitShiftPlanNoCatalogue) {
		t.Fatalf("expected ErrCommitShiftPlanNoCatalogue, got %v", err)
	}
	if len(capacity.calledFor) != 0 {
		t.Fatalf("expected no capacity query without a catalogue, got %v", capacity.calledFor)
	}
}

func setupCertifiedAssociate(t *testing.T, f *fixtures, id shared.AssociateId, certs ...shared.Certification) {
	t.Helper()
	start := &StartAssociateShift{Associates: f.associates, Events: f.pub, Clock: f.clock}
	if _, err := start.Execute(context.Background(), id, certs); err != nil {
		t.Fatalf("unexpected error setting up associate: %v", err)
	}
}

func TestAssignLabor_Succeeds(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")

	uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	la, err := uc.Execute(context.Background(), "assoc-1", "pack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pathId, active := la.ActivePathId()
	if !active || pathId != "pack" {
		t.Fatalf("expected active assignment to pack, got %v active=%v", pathId, active)
	}

	count, err := f.assignments.CountActiveByPath(context.Background(), "pack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 active assignment on pack, got %d", count)
	}
}

// TestAssignLabor_RejectsMissingCertification is a Definition-of-Done named
// failing-path test at the application layer.
func TestAssignLabor_RejectsMissingCertification(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")

	uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	_, err := uc.Execute(context.Background(), "assoc-1", "hazmat")
	if !errors.Is(err, assignment.ErrCertificationRequired) {
		t.Fatalf("expected ErrCertificationRequired, got %v", err)
	}
}

// TestAssignLabor_CatalogueNormalisesRequiredCertification is the 2026-10
// ADR-conformance regression test (ADR-0009 amendment): with a catalogue
// wired, the required certification is the path FAMILY's canonical prefix,
// so a catalogue-valid "PICK" or "pick-zone-a" must satisfy a "pick"
// certification — the pre-fix exact raw-id match rejected both.
func TestAssignLabor_CatalogueNormalisesRequiredCertification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pathId shared.PathId
	}{
		{name: "canonical upper-case id", pathId: "PICK"},
		{name: "bare lower-case prefix", pathId: "pick"},
		{name: "zone-suffixed real-fleet id", pathId: "pick-zone-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtures()
			setupCertifiedAssociate(t, f, "assoc-1", "pick")

			uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8, Catalogue: testCatalogue()}
			la, err := uc.Execute(context.Background(), "assoc-1", tc.pathId)
			if err != nil {
				t.Fatalf("catalogue-valid %q with certification \"pick\": unexpected error: %v", tc.pathId, err)
			}
			if got, active := la.ActivePathId(); !active || got != tc.pathId {
				t.Fatalf("expected active assignment to %v, got %v active=%v", tc.pathId, got, active)
			}
		})
	}

	t.Run("an associate certified for a different family is still rejected", func(t *testing.T) {
		f := newFixtures()
		setupCertifiedAssociate(t, f, "assoc-2", "pack")

		uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8, Catalogue: testCatalogue()}
		if _, err := uc.Execute(context.Background(), "assoc-2", "pick-zone-a"); !errors.Is(err, assignment.ErrCertificationRequired) {
			t.Fatalf("expected ErrCertificationRequired, got %v", err)
		}
	})

	t.Run("without a catalogue the raw pathId remains the certification name", func(t *testing.T) {
		f := newFixtures()
		setupCertifiedAssociate(t, f, "assoc-3", "pick")

		uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
		if _, err := uc.Execute(context.Background(), "assoc-3", "PICK"); !errors.Is(err, assignment.ErrCertificationRequired) {
			t.Fatalf("catalogue-less configuration must keep the exact raw-id convention, got %v", err)
		}
	})
}

// TestAssignLabor_HazmatPath is an illustrative test, not a new behaviour.
// It applies the ALREADY-EXISTING path-name-equals-certification-name gate
// (documented in ADR 0003 and internal/application/usecases/assign_labor.go)
// to a hazmat-designated path specifically, since "hazmat" is a real,
// documented Certification value (see CLAUDE.md's Ubiquitous Language and
// docs/docs/business-context/ubiquitous-language.md) and not a hypothetical
// one. No new aggregate, port, or use-case logic is introduced by this test
// or by ADR 0009 — the same generic certified/uncertified assertions as
// TestAssignLabor_Succeeds and TestAssignLabor_RejectsMissingCertification
// above, run against pathId "hazmat" instead of "pack".
func TestAssignLabor_HazmatPath(t *testing.T) {
	t.Run("rejects an associate without the hazmat certification", func(t *testing.T) {
		f := newFixtures()
		setupCertifiedAssociate(t, f, "assoc-1", "pack")

		uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
		_, err := uc.Execute(context.Background(), "assoc-1", "hazmat")
		if !errors.Is(err, assignment.ErrCertificationRequired) {
			t.Fatalf("expected ErrCertificationRequired, got %v", err)
		}
	})

	t.Run("accepts an associate holding the hazmat certification", func(t *testing.T) {
		f := newFixtures()
		setupCertifiedAssociate(t, f, "assoc-2", "hazmat")

		uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
		la, err := uc.Execute(context.Background(), "assoc-2", "hazmat")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		pathId, active := la.ActivePathId()
		if !active || pathId != "hazmat" {
			t.Fatalf("expected active assignment to hazmat, got %v active=%v", pathId, active)
		}
	})
}

// TestAssignLabor_RejectsWhileOnBreak is a Definition-of-Done named
// failing-path test at the application layer.
func TestAssignLabor_RejectsWhileOnBreak(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")

	startBreak := &StartBreak{Associates: f.associates, Events: f.pub, Clock: f.clock}
	if err := startBreak.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	_, err := uc.Execute(context.Background(), "assoc-1", "pack")
	if !errors.Is(err, associate.ErrOnBreak) {
		t.Fatalf("expected ErrOnBreak, got %v", err)
	}
}

func TestAssignLabor_SecondAssignmentEndsPriorAndLogsHours(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack", "stow")

	uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if _, err := uc.Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f.clock.now = f.clock.now.Add(3 * time.Hour)
	la, err := uc.Execute(context.Background(), "assoc-1", "stow")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pathId, active := la.ActivePathId()
	if !active || pathId != "stow" {
		t.Fatalf("expected single active assignment to stow, got %v active=%v", pathId, active)
	}

	countPack, _ := f.assignments.CountActiveByPath(context.Background(), "pack")
	if countPack != 0 {
		t.Fatalf("expected 0 active assignments on pack after reassignment, got %d", countPack)
	}

	stored, _ := f.associates.FindByID(context.Background(), "assoc-1")
	if stored.HoursLogged() != 3 {
		t.Fatalf("expected 3 hours logged from closed pack interval, got %v", stored.HoursLogged())
	}
}

func TestStartBreak_And_EndBreak(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")

	startBreak := &StartBreak{Associates: f.associates, Events: f.pub, Clock: f.clock}
	endBreak := &EndBreak{Associates: f.associates, Events: f.pub, Clock: f.clock}

	if err := startBreak.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stored, _ := f.associates.FindByID(context.Background(), "assoc-1")
	if !stored.IsOnBreak() {
		t.Fatal("expected associate to be on break")
	}

	if err := endBreak.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stored, _ = f.associates.FindByID(context.Background(), "assoc-1")
	if stored.IsOnBreak() {
		t.Fatal("expected associate to no longer be on break")
	}
}

func TestGetStaffingGap_RaisesPathUnderstaffed(t *testing.T) {
	f := newFixtures()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 24}}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}
	gap, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gap.Understaffed || gap.PlannedHeads != 3 || gap.ActiveHeads != 0 {
		t.Fatalf("unexpected gap: %+v", gap)
	}

	found := false
	for _, e := range f.pub.Events() {
		if e.EventName() == "PathUnderstaffed" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected PathUnderstaffed to be published")
	}
}

func TestEndAssociateShift_ClosesActiveAssignmentAndShift(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")

	assign := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if _, err := assign.Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f.clock.now = f.clock.now.Add(5 * time.Hour)
	end := &EndAssociateShift{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if err := end.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stored, _ := f.associates.FindByID(context.Background(), "assoc-1")
	if !stored.Ended() {
		t.Fatal("expected shift to be ended")
	}
	if stored.HoursLogged() != 5 {
		t.Fatalf("expected 5 hours logged, got %v", stored.HoursLogged())
	}

	count, _ := f.assignments.CountActiveByPath(context.Background(), "pack")
	if count != 0 {
		t.Fatalf("expected 0 active assignments after shift end, got %d", count)
	}
}

func TestStartAssociateShift_SaveError(t *testing.T) {
	f := newFixtures()
	repo := &failingAssociateRepo{AssociateRepo: f.associates, saveErr: errBoom}
	uc := &StartAssociateShift{Associates: repo, Events: f.pub, Clock: f.clock}
	_, err := uc.Execute(context.Background(), "assoc-1", nil)
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestStartAssociateShift_PublishError(t *testing.T) {
	f := newFixtures()
	pub := &failingPublisher{LogPublisher: f.pub, err: errBoom}
	uc := &StartAssociateShift{Associates: f.associates, Events: pub, Clock: f.clock}
	_, err := uc.Execute(context.Background(), "assoc-1", nil)
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

// TestStartAssociateShift_RestartCarriesOverExistingVersion proves the
// idempotent-restart contract survives ADR 0021's version guard: calling
// StartAssociateShift twice for the SAME associate (documented in
// apis/openapi.yaml as upserting the roster entry, not erroring) must
// succeed on the second call too, not be rejected as a stale write
// against the fresh version-1 aggregate NewAssociateShift always builds.
func TestStartAssociateShift_RestartCarriesOverExistingVersion(t *testing.T) {
	f := newFixtures()
	uc := &StartAssociateShift{Associates: f.associates, Events: f.pub, Clock: f.clock}

	if _, err := uc.Execute(context.Background(), "assoc-1", []shared.Certification{"pack"}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	first, err := f.associates.FindByID(context.Background(), "assoc-1")
	if err != nil {
		t.Fatalf("load after first start: %v", err)
	}
	if first.Version() != 1 {
		t.Fatalf("expected version 1 after first start, got %d", first.Version())
	}

	// A second call restarts the roster entry -- must succeed, not
	// return ports.ErrConcurrentModification.
	if _, err := uc.Execute(context.Background(), "assoc-1", []shared.Certification{"stow"}); err != nil {
		t.Fatalf("restart (second start) must succeed per the documented upsert contract, got %v", err)
	}
	second, err := f.associates.FindByID(context.Background(), "assoc-1")
	if err != nil {
		t.Fatalf("load after restart: %v", err)
	}
	if second.Version() != 2 {
		t.Fatalf("expected version to advance to 2 after the restart, got %d", second.Version())
	}
	if !second.HasCertification("stow") {
		t.Fatal("expected the restart's certifications to have been applied")
	}
}

func TestCertifyAssociate_SaveError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	repo := &failingAssociateRepo{AssociateRepo: f.associates, saveErr: errBoom}
	uc := &CertifyAssociate{Associates: repo, Events: f.pub, Clock: f.clock}
	err := uc.Execute(context.Background(), "assoc-1", "hazmat")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestCertifyAssociate_PublishError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	pub := &failingPublisher{LogPublisher: f.pub, err: errBoom}
	uc := &CertifyAssociate{Associates: f.associates, Events: pub, Clock: f.clock}
	err := uc.Execute(context.Background(), "assoc-1", "hazmat")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestProposePathPlan_PublishError(t *testing.T) {
	f := newFixtures()
	pub := &failingPublisher{LogPublisher: f.pub, err: errBoom}
	uc := &ProposePathPlan{Events: pub, Clock: f.clock}
	_, _, _, _, err := uc.Execute(context.Background(), "bldg-1", "pack", 100, 30)
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestCommitShiftPlan_SaveError(t *testing.T) {
	f := newFixtures()
	repo := &failingShiftPlanRepo{ShiftPlanRepo: f.shiftPlans, saveErr: errBoom}
	uc := &CommitShiftPlan{ShiftPlans: repo, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 10}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40}}
	installed := map[shared.PathId]int{"pack": 10}

	_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, installed)
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestCommitShiftPlan_PublishError(t *testing.T) {
	f := newFixtures()
	pub := &failingPublisher{LogPublisher: f.pub, err: errBoom}
	uc := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 10}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}

	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 5, PlannedRate: 30, PlannedHours: 40}}
	installed := map[shared.PathId]int{"pack": 10}

	_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", lines, installed)
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestAssignLabor_AssociateNotFound(t *testing.T) {
	f := newFixtures()
	uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	_, err := uc.Execute(context.Background(), "ghost", "pack")
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestAssignLabor_FindAssignmentError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")
	repo := &failingAssignmentRepo{AssignmentRepo: f.assignments, findErr: errBoom}
	uc := &AssignLabor{Associates: f.associates, Assignments: repo, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	_, err := uc.Execute(context.Background(), "assoc-1", "pack")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

// TestAssignLabor_LogHoursExceedsMax exercises shift.LogHours failing from
// inside AssignLabor.Execute (a reassignment closing a too-long interval) —
// distinct from the domain-level LogHours tests in associate_shift_test.go.
func TestAssignLabor_LogHoursExceedsMax(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack", "stow")

	uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 2}
	if _, err := uc.Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f.clock.now = f.clock.now.Add(3 * time.Hour)
	_, err := uc.Execute(context.Background(), "assoc-1", "stow")
	if !errors.Is(err, associate.ErrMaxHoursExceeded) {
		t.Fatalf("expected ErrMaxHoursExceeded, got %v", err)
	}
}

func TestAssignLabor_AssociatesSaveError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack", "stow")

	first := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if _, err := first.Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f.clock.now = f.clock.now.Add(1 * time.Hour)
	repo := &failingAssociateRepo{AssociateRepo: f.associates, saveErr: errBoom}
	uc := &AssignLabor{Associates: repo, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	_, err := uc.Execute(context.Background(), "assoc-1", "stow")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestAssignLabor_AssignmentsSaveError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")
	repo := &failingAssignmentRepo{AssignmentRepo: f.assignments, saveErr: errBoom}
	uc := &AssignLabor{Associates: f.associates, Assignments: repo, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	_, err := uc.Execute(context.Background(), "assoc-1", "pack")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestAssignLabor_PublishError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")
	pub := &failingPublisher{LogPublisher: f.pub, err: errBoom}
	uc := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: pub, Clock: f.clock, MaxHoursPerShift: 8}
	_, err := uc.Execute(context.Background(), "assoc-1", "pack")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestStartBreak_NotFound(t *testing.T) {
	f := newFixtures()
	uc := &StartBreak{Associates: f.associates, Events: f.pub, Clock: f.clock}
	err := uc.Execute(context.Background(), "ghost")
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStartBreak_SaveError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	repo := &failingAssociateRepo{AssociateRepo: f.associates, saveErr: errBoom}
	uc := &StartBreak{Associates: repo, Events: f.pub, Clock: f.clock}
	err := uc.Execute(context.Background(), "assoc-1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestStartBreak_PublishError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	pub := &failingPublisher{LogPublisher: f.pub, err: errBoom}
	uc := &StartBreak{Associates: f.associates, Events: pub, Clock: f.clock}
	err := uc.Execute(context.Background(), "assoc-1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestEndBreak_NotFound(t *testing.T) {
	f := newFixtures()
	uc := &EndBreak{Associates: f.associates, Events: f.pub, Clock: f.clock}
	err := uc.Execute(context.Background(), "ghost")
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestEndBreak_SaveError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	startBreak := &StartBreak{Associates: f.associates, Events: f.pub, Clock: f.clock}
	if err := startBreak.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	repo := &failingAssociateRepo{AssociateRepo: f.associates, saveErr: errBoom}
	uc := &EndBreak{Associates: repo, Events: f.pub, Clock: f.clock}
	err := uc.Execute(context.Background(), "assoc-1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestEndBreak_PublishError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	startBreak := &StartBreak{Associates: f.associates, Events: f.pub, Clock: f.clock}
	if err := startBreak.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pub := &failingPublisher{LogPublisher: f.pub, err: errBoom}
	uc := &EndBreak{Associates: f.associates, Events: pub, Clock: f.clock}
	err := uc.Execute(context.Background(), "assoc-1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestGetStaffingGap_PlanNotFound(t *testing.T) {
	f := newFixtures()
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}
	_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGetStaffingGap_CountActiveError(t *testing.T) {
	f := newFixtures()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 24}}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	repo := &failingAssignmentRepo{AssignmentRepo: f.assignments, countErr: errBoom}
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: repo, Events: f.pub, Clock: f.clock}
	_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestGetStaffingGap_PublishErrorWhenUnderstaffed(t *testing.T) {
	f := newFixtures()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 24}}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pub := &failingPublisher{LogPublisher: f.pub, err: errBoom}
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: pub, Clock: f.clock}
	_, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestGetStaffingGap_NotUnderstaffed(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")
	assign := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if _, err := assign.Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 1, PlannedRate: 30, PlannedHours: 8}}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}
	gap, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gap.Understaffed {
		t.Fatalf("expected not understaffed, got %+v", gap)
	}

	for _, e := range f.pub.Events() {
		if e.EventName() == "PathUnderstaffed" {
			t.Fatal("did not expect PathUnderstaffed to be published")
		}
	}
}

// --- idleness-as-staffing-signal: GetStaffingGap surfacing ------------------

// TestGetStaffingGap_ObservedIdlePctSurfaced covers pure surfacing: when
// IdleShare reports a value, it appears verbatim on the response, with
// no change to the existing gap computation.
func TestGetStaffingGap_ObservedIdlePctSurfaced(t *testing.T) {
	f := newFixtures()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 8}}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idleClient := &fakeIdleShareClient{share: 0.42}
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock, IdleShare: idleClient}
	gap, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gap.ObservedIdlePct == nil {
		t.Fatal("expected ObservedIdlePct to be non-nil when IdleShare reports a value")
	}
	if *gap.ObservedIdlePct != 0.42 {
		t.Fatalf("ObservedIdlePct = %v, want 0.42", *gap.ObservedIdlePct)
	}
}

// TestGetStaffingGap_ObservedIdlePctNilWhenUnwired covers the default,
// permissive configuration (no LABOR_PERFORMANCE_MODE=kafka-cache
// wired): ObservedIdlePct must be nil, never a fabricated 0.
func TestGetStaffingGap_ObservedIdlePctNilWhenUnwired(t *testing.T) {
	f := newFixtures()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 8}}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock} // IdleShare left nil
	gap, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gap.ObservedIdlePct != nil {
		t.Fatalf("expected nil ObservedIdlePct when IdleShare is unwired, got %v", *gap.ObservedIdlePct)
	}
}

// TestGetStaffingGap_ObservedIdlePctNilOnErrIdleShareUnavailable covers
// the no-data-yet / no-TaskType-mapping case: ObservedIdlePct must be
// nil, and the error must never fail Execute (pure surfacing).
func TestGetStaffingGap_ObservedIdlePctNilOnErrIdleShareUnavailable(t *testing.T) {
	f := newFixtures()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 8}}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	idleClient := &fakeIdleShareClient{err: ports.ErrIdleShareUnavailable}
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock, IdleShare: idleClient}
	gap, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gap.ObservedIdlePct != nil {
		t.Fatalf("expected nil ObservedIdlePct on ErrIdleShareUnavailable, got %v", *gap.ObservedIdlePct)
	}
}

// --- ADR-0011 fast-follow: fleet-wide staffing-gap list -----------------

// assertPathGap pins one planned path's staffing gap from an ExecuteAll
// result: present in byPath, with exactly the wanted understaffed flag,
// planned heads, and active heads.
func assertPathGap(t *testing.T, byPath map[shared.PathId]StaffingGap, path string, wantUnderstaffed bool, wantPlanned, wantActive int) {
	t.Helper()
	got, ok := byPath[shared.PathId(path)]
	if !ok || got.Understaffed != wantUnderstaffed || got.PlannedHeads != wantPlanned || got.ActiveHeads != wantActive {
		t.Fatalf("unexpected %s gap: %+v (ok=%v)", path, got, ok)
	}
}

// TestGetStaffingGap_ExecuteAll_ReturnsGapForEveryPlannedPath proves the
// list endpoint's use case reuses the exact same per-path computation as
// Execute (same PlannedHeads/ActiveHeads/Understaffed for a given path,
// gathered for every line in the committed plan in one call).
func TestGetStaffingGap_ExecuteAll_ReturnsGapForEveryPlannedPath(t *testing.T) {
	f := newFixtures()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5, "pick": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 3, PlannedRate: 30, PlannedHours: 24},
		{PathId: "pick", PlannedHeads: 1, PlannedRate: 25, PlannedHours: 8},
	}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5, "pick": 5}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	setupCertifiedAssociate(t, f, "assoc-1", "pick")
	if _, err := (&AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}).Execute(context.Background(), "assoc-1", "pick"); err != nil {
		t.Fatalf("setup assign: %v", err)
	}

	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}
	gaps, err := uc.ExecuteAll(context.Background(), "bldg-1", "shift-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gaps) != 2 {
		t.Fatalf("expected 2 gaps (one per planned path), got %d: %+v", len(gaps), gaps)
	}

	byPath := map[shared.PathId]StaffingGap{}
	for _, g := range gaps {
		byPath[g.PathId] = g
	}
	pack := byPath["pack"]
	assertPathGap(t, byPath, "pack", true, 3, 0)
	assertPathGap(t, byPath, "pick", false, 1, 1)

	// Cross-check against the single-path Execute for the same inputs --
	// the two must never drift, since ExecuteAll reuses Execute's exact
	// per-path core.
	single, err := uc.Execute(context.Background(), "bldg-1", "shift-1", "pack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if single != pack {
		t.Fatalf("ExecuteAll's pack gap %+v diverges from Execute's %+v", pack, single)
	}
}

// TestGetStaffingGap_ExecuteAll_PublishesEveryUnderstaffedPathInOneScope
// proves every PathUnderstaffed event across the whole plan is raised, and
// that they are batched into ONE UnitOfWork scope rather than one per
// line -- mirroring the outbox's atomic-batch discipline (ADR 0016).
func TestGetStaffingGap_ExecuteAll_PublishesEveryUnderstaffedPathInOneScope(t *testing.T) {
	f := newFixtures()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: &scopedPublisher{}, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5, "pick": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 2, PlannedRate: 30, PlannedHours: 16},
		{PathId: "pick", PlannedHeads: 1, PlannedRate: 25, PlannedHours: 8},
	}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5, "pick": 5}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	pub := &scopedPublisher{}
	uow := &recordingUnitOfWork{}
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: pub, Clock: f.clock, UnitOfWork: uow}
	gaps, err := uc.ExecuteAll(context.Background(), "bldg-1", "shift-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gaps) != 2 {
		t.Fatalf("expected 2 gaps, got %d", len(gaps))
	}
	// Both lines are understaffed (0 active each) -- exactly one scope,
	// carrying both events, not two separate scopes.
	assertOneCommittedScope(t, uow, pub)
	if pub.events != 2 {
		t.Fatalf("expected 2 PathUnderstaffed events published in the one scope, got %d", pub.events)
	}
}

// TestGetStaffingGap_ExecuteAll_PlanNotFound mirrors Execute's own
// not-found behavior for the same missing (buildingId, shiftId).
func TestGetStaffingGap_ExecuteAll_PlanNotFound(t *testing.T) {
	f := newFixtures()
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock}
	if _, err := uc.ExecuteAll(context.Background(), "bldg-1", "shift-1"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestGetStaffingGap_ExecuteAll_NoUnderstaffedPaths_OpensNoScope mirrors
// Execute's "a fully staffed path opens no scope" behavior across every
// line in the plan.
func TestGetStaffingGap_ExecuteAll_NoUnderstaffedPaths_OpensNoScope(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")
	if _, err := (&AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}).Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("setup assign: %v", err)
	}
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 1, PlannedRate: 30, PlannedHours: 8}}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	uow := &recordingUnitOfWork{}
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: f.pub, Clock: f.clock, UnitOfWork: uow}
	gaps, err := uc.ExecuteAll(context.Background(), "bldg-1", "shift-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gaps) != 1 || gaps[0].Understaffed {
		t.Fatalf("expected 1 fully-staffed gap, got %+v", gaps)
	}
	if uow.opened != 0 {
		t.Fatalf("no path understaffed must open no scope, got opened=%d", uow.opened)
	}
}

func TestEndAssociateShift_AssociateNotFound(t *testing.T) {
	f := newFixtures()
	uc := &EndAssociateShift{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	err := uc.Execute(context.Background(), "ghost")
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestEndAssociateShift_FindAssignmentError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")
	repo := &failingAssignmentRepo{AssignmentRepo: f.assignments, findErr: errBoom}
	uc := &EndAssociateShift{Associates: f.associates, Assignments: repo, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	err := uc.Execute(context.Background(), "assoc-1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

// TestEndAssociateShift_LogHoursExceedsMax exercises shift.LogHours failing
// from inside EndAssociateShift.Execute when closing the active assignment.
func TestEndAssociateShift_LogHoursExceedsMax(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")

	assign := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if _, err := assign.Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f.clock.now = f.clock.now.Add(3 * time.Hour)
	end := &EndAssociateShift{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 2}
	err := end.Execute(context.Background(), "assoc-1")
	if !errors.Is(err, associate.ErrMaxHoursExceeded) {
		t.Fatalf("expected ErrMaxHoursExceeded, got %v", err)
	}
}

func TestEndAssociateShift_AssignmentsSaveError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")

	assign := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if _, err := assign.Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f.clock.now = f.clock.now.Add(1 * time.Hour)
	repo := &failingAssignmentRepo{AssignmentRepo: f.assignments, saveErr: errBoom}
	end := &EndAssociateShift{Associates: f.associates, Assignments: repo, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	err := end.Execute(context.Background(), "assoc-1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestEndAssociateShift_AssociatesSaveError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")

	assign := &AssignLabor{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if _, err := assign.Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f.clock.now = f.clock.now.Add(1 * time.Hour)
	repo := &failingAssociateRepo{AssociateRepo: f.associates, saveErr: errBoom}
	end := &EndAssociateShift{Associates: repo, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	err := end.Execute(context.Background(), "assoc-1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

func TestEndAssociateShift_PublishError(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")

	pub := &failingPublisher{LogPublisher: f.pub, err: errBoom}
	end := &EndAssociateShift{Associates: f.associates, Assignments: f.assignments, Events: pub, Clock: f.clock, MaxHoursPerShift: 8}
	err := end.Execute(context.Background(), "assoc-1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
}

// TestEndAssociateShift_NoActiveAssignment covers an associate who was
// started but never assigned to a path: Assignments.FindByAssociateID
// returns ports.ErrNotFound, la stays nil, and the shift still ends cleanly.
func TestEndAssociateShift_NoActiveAssignment(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")

	end := &EndAssociateShift{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if err := end.Execute(context.Background(), "assoc-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stored, _ := f.associates.FindByID(context.Background(), "assoc-1")
	if !stored.Ended() {
		t.Fatal("expected shift to be ended")
	}
}

// --- coverage-gap closures: domain rejections and per-path failures ---------

// endShiftOf ends an associate's shift through the use case, mirroring how a
// real composition reaches the ended state.
func endShiftOf(t *testing.T, f *fixtures, id shared.AssociateId) {
	t.Helper()
	end := &EndAssociateShift{Associates: f.associates, Assignments: f.assignments, Events: f.pub, Clock: f.clock, MaxHoursPerShift: 8}
	if err := end.Execute(context.Background(), id); err != nil {
		t.Fatalf("setup end shift: %v", err)
	}
}

// TestCertifyAssociate_RejectsEndedShift covers the domain-rejection branch
// of Execute: CertifyAssociate on a shift that already ended must surface
// associate.ErrShiftEnded without saving or publishing anything new.
func TestCertifyAssociate_RejectsEndedShift(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1")
	endShiftOf(t, f, "assoc-1")
	eventsBefore := len(f.pub.Events())

	uc := &CertifyAssociate{Associates: f.associates, Events: f.pub, Clock: f.clock}
	err := uc.Execute(context.Background(), "assoc-1", "hazmat")
	if !errors.Is(err, associate.ErrShiftEnded) {
		t.Fatalf("expected ErrShiftEnded, got %v", err)
	}
	if got := len(f.pub.Events()); got != eventsBefore {
		t.Fatalf("a rejected certification must publish nothing, got %d new events", got-eventsBefore)
	}
	stored, _ := f.associates.FindByID(context.Background(), "assoc-1")
	if stored.HasCertification("hazmat") {
		t.Fatal("expected no certification to be granted on a rejected execute")
	}
}

// TestCertifyAssociate_DuplicateCertification documents the current
// behaviour of re-certifying a certification the associate already holds:
// it is NOT an error — the domain inserts idempotently — but it DOES raise
// another AssociateCertified event. If that ever becomes an undesired
// duplicate signal downstream, this test is the place that pins the change.
func TestCertifyAssociate_DuplicateCertification(t *testing.T) {
	f := newFixtures()
	setupCertifiedAssociate(t, f, "assoc-1", "pack")
	eventsBefore := len(f.pub.Events())

	uc := &CertifyAssociate{Associates: f.associates, Events: f.pub, Clock: f.clock}
	if err := uc.Execute(context.Background(), "assoc-1", "pack"); err != nil {
		t.Fatalf("re-certifying an existing certification must not error, got %v", err)
	}

	stored, _ := f.associates.FindByID(context.Background(), "assoc-1")
	if !stored.HasCertification("pack") {
		t.Fatal("expected the existing certification to still be held")
	}
	certifiedEvents := 0
	for _, e := range f.pub.Events()[eventsBefore:] {
		if e.EventName() == "AssociateCertified" {
			certifiedEvents++
		}
	}
	if certifiedEvents != 1 {
		t.Fatalf("expected exactly one AssociateCertified event on duplicate certify, got %d", certifiedEvents)
	}
}

// TestStartBreak_DomainRejections covers the domain-error branch of
// StartBreak.Execute that the in-memory happy-path tests never reach:
// breaking twice, and breaking after the shift ended. Neither may save or
// publish anything.
func TestStartBreak_DomainRejections(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *fixtures)
		want  error
	}{
		{
			name: "already on break",
			setup: func(t *testing.T, f *fixtures) {
				if err := (&StartBreak{Associates: f.associates, Events: f.pub, Clock: f.clock}).Execute(context.Background(), "assoc-1"); err != nil {
					t.Fatalf("setup first break: %v", err)
				}
			},
			want: associate.ErrAlreadyOnBreak,
		},
		{
			name: "shift already ended",
			setup: func(t *testing.T, f *fixtures) {
				endShiftOf(t, f, "assoc-1")
			},
			want: associate.ErrShiftEnded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtures()
			setupCertifiedAssociate(t, f, "assoc-1")
			tc.setup(t, f)
			eventsBefore := len(f.pub.Events())

			uc := &StartBreak{Associates: f.associates, Events: f.pub, Clock: f.clock}
			if err := uc.Execute(context.Background(), "assoc-1"); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if got := len(f.pub.Events()); got != eventsBefore {
				t.Fatalf("a rejected break must publish nothing, got %d new events", got-eventsBefore)
			}
		})
	}
}

// TestEndBreak_DomainRejections covers the domain-error branch of
// EndBreak.Execute: ending a break never started, and ending one after the
// shift ended. Neither may save or publish anything.
func TestEndBreak_DomainRejections(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *fixtures)
		want  error
	}{
		{
			name:  "not on break",
			setup: func(t *testing.T, f *fixtures) {},
			want:  associate.ErrNotOnBreak,
		},
		{
			name: "shift already ended",
			setup: func(t *testing.T, f *fixtures) {
				endShiftOf(t, f, "assoc-1")
			},
			want: associate.ErrShiftEnded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtures()
			setupCertifiedAssociate(t, f, "assoc-1")
			tc.setup(t, f)
			eventsBefore := len(f.pub.Events())

			uc := &EndBreak{Associates: f.associates, Events: f.pub, Clock: f.clock}
			if err := uc.Execute(context.Background(), "assoc-1"); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if got := len(f.pub.Events()); got != eventsBefore {
				t.Fatalf("a rejected break end must publish nothing, got %d new events", got-eventsBefore)
			}
		})
	}
}

// pathFailingAssignmentRepo fails CountActiveByPath only for the scripted
// paths, delegating everything else to the wrapped in-memory repo — used to
// fail one line of a multi-line plan mid-loop without failing the first.
type pathFailingAssignmentRepo struct {
	*memory.AssignmentRepo
	countErrFor map[shared.PathId]error
}

func (r *pathFailingAssignmentRepo) CountActiveByPath(ctx context.Context, pathId shared.PathId) (int, error) {
	if err, ok := r.countErrFor[pathId]; ok {
		return 0, err
	}
	return r.AssignmentRepo.CountActiveByPath(ctx, pathId)
}

// commitTwoPathPlan commits an understaffed two-line plan (pack, pick) for
// ExecuteAll tests.
func commitTwoPathPlan(t *testing.T, f *fixtures) {
	t.Helper()
	commit := &CommitShiftPlan{ShiftPlans: f.shiftPlans, Events: f.pub, Clock: f.clock, InstalledCapacity: &fakeInstalledCapacityClient{capacityByCapability: map[shared.Capability]int{"pack": 5, "pick": 5}}, Catalogue: testCatalogue(), MaxHoursPerShift: 8}
	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 2, PlannedRate: 30, PlannedHours: 16},
		{PathId: "pick", PlannedHeads: 1, PlannedRate: 25, PlannedHours: 8},
	}
	if _, err := commit.Execute(context.Background(), "bldg-1", "shift-1", lines, map[shared.PathId]int{"pack": 5, "pick": 5}); err != nil {
		t.Fatalf("setup commit: %v", err)
	}
}

// TestGetStaffingGap_ExecuteAll_PerPathErrorDiscardsPartialResults proves a
// per-path failure mid-plan aborts the whole list call: the gap already
// computed for the healthy line is discarded and nothing is published.
func TestGetStaffingGap_ExecuteAll_PerPathErrorDiscardsPartialResults(t *testing.T) {
	f := newFixtures()
	commitTwoPathPlan(t, f)

	repo := &pathFailingAssignmentRepo{AssignmentRepo: f.assignments, countErrFor: map[shared.PathId]error{"pick": errBoom}}
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: repo, Events: f.pub, Clock: f.clock}
	eventsBefore := len(f.pub.Events())

	gaps, err := uc.ExecuteAll(context.Background(), "bldg-1", "shift-1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	if gaps != nil {
		t.Fatalf("a per-path failure must discard partial results, got %+v", gaps)
	}
	for _, e := range f.pub.Events()[eventsBefore:] {
		if e.EventName() == "PathUnderstaffed" {
			t.Fatal("a per-path failure must publish no PathUnderstaffed events")
		}
	}
}

// TestGetStaffingGap_ExecuteAll_PublishError mirrors Execute's
// publish-error behavior for the batched list call: a failing publish
// surfaces the error and no gap list is returned.
func TestGetStaffingGap_ExecuteAll_PublishError(t *testing.T) {
	f := newFixtures()
	commitTwoPathPlan(t, f)

	pub := &failingPublisher{LogPublisher: f.pub, err: errBoom}
	uc := &GetStaffingGap{ShiftPlans: f.shiftPlans, Assignments: f.assignments, Events: pub, Clock: f.clock}
	gaps, err := uc.ExecuteAll(context.Background(), "bldg-1", "shift-1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	if gaps != nil {
		t.Fatalf("a failed publish must return no gaps, got %+v", gaps)
	}
}

// TestProposePathPlan_IdleShareTrimBoundaries covers the arithmetic edges of
// applyIdleShareTrim beyond the threshold cases in
// TestProposePathPlan_IdleShareTrim: zero observed idle share, a full trim
// clamped at the 1-head floor, a trim that Ceil rounds back up to the
// original headcount (a no-op that must NOT report a trim reason), and a
// trim that Ceil rounds up to the next integer.
func TestProposePathPlan_IdleShareTrimBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		heads     int // via charge/rate: charge=heads*10, rate=10
		idleShare float64
		threshold float64 // 0 = default (0.30)
		wantHeads int
		trimmed   bool
	}{
		{
			name:      "zero idle share never trims",
			heads:     10,
			idleShare: 0.0,
			wantHeads: 10,
		},
		{
			name:      "full idle share clamps at the 1-head floor",
			heads:     2,
			idleShare: 1.0,
			wantHeads: 1,
			trimmed:   true,
		},
		{
			name:      "trim Ceil-rounded back up to full heads is a no-op",
			heads:     10,
			idleShare: 0.05, // ceil(10*0.95) = ceil(9.5) = 10 = heads
			threshold: 0.04, // share must exceed the threshold to arm the trim
			wantHeads: 10,
		},
		{
			name:      "trim rounds up to the next integer",
			heads:     10,
			idleShare: 0.31, // ceil(10*0.69) = ceil(6.9) = 7
			wantHeads: 7,
			trimmed:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtures()
			idleClient := &fakeIdleShareClient{share: tc.idleShare}
			uc := &ProposePathPlan{Events: f.pub, Clock: f.clock, IdleShare: idleClient, IdleShareTrimThreshold: tc.threshold}

			charge := float64(tc.heads) * 10
			heads, _, _, trimReason, err := uc.Execute(context.Background(), "bldg-1", "pack", charge, 10)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !idleClient.called {
				t.Fatal("expected IdleShare to be consulted")
			}
			if heads != tc.wantHeads {
				t.Fatalf("heads = %d, want %d", heads, tc.wantHeads)
			}
			if tc.trimmed && trimReason == "" {
				t.Fatal("expected a non-empty trimReason when a trim was applied")
			}
			if !tc.trimmed && trimReason != "" {
				t.Fatalf("expected no trim, got trimReason %q", trimReason)
			}
		})
	}
}
