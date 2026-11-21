package bdd

// Step definitions for the documentation-derived rule features
// (*_doc_rules.feature). They extend the shared world with: a movable clock
// (hours logged by closed assignment intervals), a labor-performance measured
// rate, an idle-share signal that can be unavailable, a fulfillment-execution
// that answers per capability (and counts how often it was asked), restarts of
// a roster entry, paging by the returned cursor and typed assertions on the
// domain events the service published.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cucumber/godog"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// capacityProbe wraps the fulfillment-execution double: it counts every
// lookup per capability and can fail for chosen capabilities only.
type capacityProbe struct {
	inner ports.InstalledCapacityClient
	down  map[shared.Capability]bool

	mu    sync.Mutex
	calls map[shared.Capability]int
}

func (p *capacityProbe) InstalledCapacity(ctx context.Context, capability shared.Capability) (int, error) {
	p.mu.Lock()
	p.calls[capability]++
	failing := p.down[capability]
	p.mu.Unlock()
	if failing {
		return 0, fmt.Errorf("%w: connection refused for capability %q", ports.ErrInstalledCapacityUnavailable, capability)
	}
	return p.inner.InstalledCapacity(ctx, capability)
}

func (p *capacityProbe) asked(capability shared.Capability) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[capability]
}

// measuredRateFake is labor-performance's mean-seconds-per-task feed for ONE
// path (every other path has no measurement yet): either a fixed answer or
// ErrMeasuredRateUnavailable, counting how often it was asked.
type measuredRateFake struct {
	path        shared.PathId
	seconds     float64
	unavailable bool

	mu    sync.Mutex
	calls int
}

func (m *measuredRateFake) MeanActualSeconds(_ context.Context, pathId shared.PathId) (float64, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if m.unavailable || pathId != m.path {
		return 0, ports.ErrMeasuredRateUnavailable
	}
	return m.seconds, nil
}

func (m *measuredRateFake) consulted() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// noIdleShare is the idle-share cache before any observation exists.
type noIdleShare struct{}

func (noIdleShare) IdleSharePct(context.Context, shared.PathId) (float64, error) {
	return 0, ports.ErrIdleShareUnavailable
}

// --- Given ------------------------------------------------------------------

func (w *world) clockAdvancesByHours(hours float64) error {
	w.h.clock.now = w.h.clock.now.Add(time.Duration(hours * float64(time.Hour)))
	return nil
}

func (w *world) laborPerformanceReportsMean(seconds float64, path string) error {
	w.measured = &measuredRateFake{path: shared.PathId(path), seconds: seconds}
	w.rebuild()
	return nil
}

func (w *world) laborPerformanceHasNoMeasuredRate(_ string) error {
	w.measured = &measuredRateFake{unavailable: true}
	w.rebuild()
	return nil
}

func (w *world) noIdleShareObserved(_ string) error {
	w.idleShare = noIdleShare{}
	w.rebuild()
	return nil
}

func (w *world) capacityUnknownForCapability(capability string) error {
	if w.capabilityDown == nil {
		w.capabilityDown = map[shared.Capability]bool{}
	}
	w.capabilityDown[shared.Capability(capability)] = true
	w.rebuild()
	return nil
}

// associatesWorkPath puts n certified associates (no site) on a path, ids
// "<path>-worker-<i>".
func (w *world) associatesWorkPath(ctx context.Context, n int, certification, pathId string) error {
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("%s-worker-%d", pathId, i)
		if err := w.associateShiftStartedWithCertifications(ctx, id, certification); err != nil {
			return err
		}
		if err := w.associateIsAssignedToPath(ctx, id, pathId); err != nil {
			return err
		}
		if err := w.expectStatus(http.StatusCreated); err != nil {
			return err
		}
	}
	return nil
}

func (w *world) associateRestartsShift(ctx context.Context, associateId, certs string) error {
	return w.associateShiftStartedWithCertifications(ctx, associateId, certs)
}

func (w *world) associateRestartsShiftAtSite(ctx context.Context, associateId, siteCode, certs string) error {
	return w.associateShiftStartedAtSite(ctx, associateId, siteCode, certs)
}

// labourPlanPublishedAtVersion stores one LabourPlanCommitted row carrying
// an explicit version in the scenario's copy (ADR 0043 decision 1), exactly
// as the labour-plan consumer would apply it — the repo's Supersedes guard
// decides whether it actually replaces the stored row.
func (w *world) labourPlanPublishedAtVersion(heads int, rate, site, shift, pathId string, version int) error {
	r, err := parseFloat(rate)
	if err != nil {
		return err
	}
	return w.h.requirements.Save(context.Background(), labourplan.Requirement{
		Key:              labourplan.Key{SiteCode: site, ShiftId: shift, PathId: shared.PathId(pathId)},
		PlannedHeads:     heads,
		RateUnitsPerHour: r,
		Hours:            8,
		HeadsSource:      labourplan.HeadsSourceForecast,
		Version:          version,
	})
}

// --- When -------------------------------------------------------------------

func (w *world) nextPageRequested(ctx context.Context, path string) error {
	var page struct {
		NextCursor string `json:"nextCursor"`
	}
	if err := w.decodeLast(&page); err != nil {
		return err
	}
	if page.NextCursor == "" {
		return fmt.Errorf("the last response has no nextCursor: %s", string(w.lastBody))
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return w.do(ctx, http.MethodGet, path+sep+"cursor="+page.NextCursor, nil)
}

// --- Then: response ---------------------------------------------------------

func (w *world) fieldContains(path, fragment string) error {
	got, ok, err := w.lookup(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("response has no field %q: %s", path, string(w.lastBody))
	}
	s, isString := got.(string)
	if !isString || !strings.Contains(s, fragment) {
		return fmt.Errorf("field %q is %v, want it to contain %q", path, got, fragment)
	}
	return nil
}

func (w *world) responseCarriesNextCursor() error {
	got, ok, err := w.lookup("nextCursor")
	if err != nil {
		return err
	}
	if s, isString := got.(string); !ok || !isString || s == "" {
		return fmt.Errorf("response must carry a nextCursor: %s", string(w.lastBody))
	}
	return nil
}

func (w *world) fieldsHoldSameValue(path, other string) error {
	a, okA, err := w.lookup(path)
	if err != nil {
		return err
	}
	b, okB, err := w.lookup(other)
	if err != nil {
		return err
	}
	if !okA || !okB || a != b {
		return fmt.Errorf("fields %q (%v) and %q (%v) must both be present and equal: %s", path, a, other, b, string(w.lastBody))
	}
	return nil
}

// --- Then: collaborators ----------------------------------------------------

func (w *world) fulfillmentExecutionAskedTimes(capability string, want int) error {
	if got := w.probe.asked(shared.Capability(capability)); got != want {
		return fmt.Errorf("fulfillment-execution was asked about capability %q %d times, want %d", capability, got, want)
	}
	return nil
}

func (w *world) laborPerformanceConsultedTimes(want int) error {
	m, ok := w.measured.(*measuredRateFake)
	if !ok {
		return fmt.Errorf("no labor-performance double is wired in this scenario")
	}
	if got := m.consulted(); got != want {
		return fmt.Errorf("labor-performance was consulted %d times, want %d", got, want)
	}
	return nil
}

// --- Then: typed events ------------------------------------------------------

// lastEventNamed returns the newest published event with the given name.
func (w *world) lastEventNamed(name string) (shared.DomainEvent, error) {
	p := w.h.published
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.events) - 1; i >= 0; i-- {
		if p.events[i].EventName() == name {
			return p.events[i], nil
		}
	}
	return nil, fmt.Errorf("no %s event was published", name)
}

func (w *world) lastLaborAssignedIs(associateId, pathId string) error {
	ev, err := w.lastEventNamed("LaborAssigned")
	if err != nil {
		return err
	}
	e, ok := ev.(shared.LaborAssigned)
	if !ok {
		return fmt.Errorf("LaborAssigned has unexpected type %T", ev)
	}
	if string(e.AssociateId) != associateId || string(e.PathId) != pathId {
		return fmt.Errorf("LaborAssigned is %s on %s, want %s on %s", e.AssociateId, e.PathId, associateId, pathId)
	}
	return nil
}

func (w *world) lastLaborReassignedIs(associateId, from, to string) error {
	ev, err := w.lastEventNamed("LaborReassigned")
	if err != nil {
		return err
	}
	e, ok := ev.(shared.LaborReassigned)
	if !ok {
		return fmt.Errorf("LaborReassigned has unexpected type %T", ev)
	}
	if string(e.AssociateId) != associateId || string(e.FromPathId) != from || string(e.ToPathId) != to {
		return fmt.Errorf("LaborReassigned is %s from %s to %s, want %s from %s to %s", e.AssociateId, e.FromPathId, e.ToPathId, associateId, from, to)
	}
	return nil
}

func (w *world) lastPathUnderstaffedIs(pathId string, planned, active int, siteCode string) error {
	ev, err := w.lastEventNamed("PathUnderstaffed")
	if err != nil {
		return err
	}
	e, ok := ev.(shared.PathUnderstaffed)
	if !ok {
		return fmt.Errorf("PathUnderstaffed has unexpected type %T", ev)
	}
	if string(e.PathId) != pathId || e.PlannedHeads != planned || e.ActiveHeads != active {
		return fmt.Errorf("PathUnderstaffed is %s planned %d active %d, want %s planned %d active %d", e.PathId, e.PlannedHeads, e.ActiveHeads, pathId, planned, active)
	}
	if string(e.SiteCode) != siteCode {
		return fmt.Errorf("PathUnderstaffed site is %q, want %q (empty means across all sites)", e.SiteCode, siteCode)
	}
	return nil
}

func (w *world) lastShiftPlanProposedIs(siteCode, pathId string, heads int, rate float64) error {
	ev, err := w.lastEventNamed("ShiftPlanProposed")
	if err != nil {
		return err
	}
	e, ok := ev.(shared.ShiftPlanProposed)
	if !ok {
		return fmt.Errorf("ShiftPlanProposed has unexpected type %T", ev)
	}
	if e.SiteCode() != siteCode || string(e.PathId) != pathId || e.PlannedHeads != heads || e.PlannedRate != rate {
		return fmt.Errorf("ShiftPlanProposed is site %s path %s heads %d rate %g, want site %s path %s heads %d rate %g",
			e.SiteCode(), e.PathId, e.PlannedHeads, e.PlannedRate, siteCode, pathId, heads, rate)
	}
	return nil
}

func (w *world) lastShiftPlanCommittedIs(siteCode, shiftId string) error {
	ev, err := w.lastEventNamed("ShiftPlanCommitted")
	if err != nil {
		return err
	}
	e, ok := ev.(shared.ShiftPlanCommitted)
	if !ok {
		return fmt.Errorf("ShiftPlanCommitted has unexpected type %T", ev)
	}
	if e.SiteCode() != siteCode || e.ShiftId != shiftId {
		return fmt.Errorf("ShiftPlanCommitted is site %s shift %s, want site %s shift %s", e.SiteCode(), e.ShiftId, siteCode, shiftId)
	}
	return nil
}

func (w *world) lastAssociateCertifiedIs(associateId, certification string) error {
	ev, err := w.lastEventNamed("AssociateCertified")
	if err != nil {
		return err
	}
	e, ok := ev.(shared.AssociateCertified)
	if !ok {
		return fmt.Errorf("AssociateCertified has unexpected type %T", ev)
	}
	if string(e.AssociateId) != associateId || string(e.Certification) != certification {
		return fmt.Errorf("AssociateCertified is %s with %s, want %s with %s", e.AssociateId, e.Certification, associateId, certification)
	}
	return nil
}

func (w *world) lastAssociateShiftStartedIs(associateId, certs string) error {
	ev, err := w.lastEventNamed("AssociateShiftStarted")
	if err != nil {
		return err
	}
	e, ok := ev.(shared.AssociateShiftStarted)
	if !ok {
		return fmt.Errorf("AssociateShiftStarted has unexpected type %T", ev)
	}
	names := make([]string, len(e.Certifications))
	for i, c := range e.Certifications {
		names[i] = string(c)
	}
	if string(e.AssociateId) != associateId || strings.Join(names, ",") != certs {
		return fmt.Errorf("AssociateShiftStarted is %s with %q, want %s with %q", e.AssociateId, strings.Join(names, ","), associateId, certs)
	}
	return nil
}

// parseFloat keeps the step wrappers below short.
func parseFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("number %q: %w", s, err)
	}
	return f, nil
}

func registerRules(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^the clock advances by ([0-9.]+) hours?$`, func(h string) error {
		hours, err := parseFloat(h)
		if err != nil {
			return err
		}
		return w.clockAdvancesByHours(hours)
	})
	sc.Step(`^labor-performance reports a mean of ([0-9.-]+) seconds per task for path "([^"]*)"$`, func(s, path string) error {
		seconds, err := parseFloat(s)
		if err != nil {
			return err
		}
		return w.laborPerformanceReportsMean(seconds, path)
	})
	sc.Step(`^labor-performance has no measured rate for path "([^"]*)"$`, w.laborPerformanceHasNoMeasuredRate)
	sc.Step(`^no idle share has been observed for path "([^"]*)"$`, w.noIdleShareObserved)
	sc.Step(`^fulfillment-execution cannot report the capacity of capability "([^"]*)"$`, w.capacityUnknownForCapability)
	sc.Step(`^(\d+) associates certified for "([^"]*)" are working path "([^"]*)"$`, w.associatesWorkPath)
	sc.Step(`^associate "([^"]*)" restarts their shift with certifications "([^"]*)"$`, w.associateRestartsShift)
	sc.Step(`^associate "([^"]*)" restarts their shift at site "([^"]*)" with certifications "([^"]*)"$`, w.associateRestartsShiftAtSite)

	sc.Step(`^the next page of "([^"]*)" is requested with the returned cursor$`, w.nextPageRequested)

	sc.Step(`^the response field "([^"]*)" contains "([^"]*)"$`, w.fieldContains)
	sc.Step(`^the response carries a nextCursor$`, w.responseCarriesNextCursor)
	sc.Step(`^the response fields "([^"]*)" and "([^"]*)" hold the same value$`, w.fieldsHoldSameValue)

	sc.Step(`^fulfillment-execution was asked about capability "([^"]*)" (\d+) times?$`, w.fulfillmentExecutionAskedTimes)
	sc.Step(`^labor-performance was consulted (\d+) times?$`, w.laborPerformanceConsultedTimes)

	sc.Step(`^the last LaborAssigned event placed associate "([^"]*)" on path "([^"]*)"$`, w.lastLaborAssignedIs)
	sc.Step(`^the last LaborReassigned event moved associate "([^"]*)" from path "([^"]*)" to path "([^"]*)"$`, w.lastLaborReassignedIs)
	sc.Step(`^the last PathUnderstaffed event is for path "([^"]*)" with (\d+) planned and (\d+) active heads (?:at site "([^"]*)"|across all sites)$`, w.lastPathUnderstaffedIs)
	sc.Step(`^the last ShiftPlanProposed event is for site "([^"]*)" path "([^"]*)" with (\d+) heads at rate ([0-9.]+)$`, func(site, path string, heads int, r string) error {
		rate, err := parseFloat(r)
		if err != nil {
			return err
		}
		return w.lastShiftPlanProposedIs(site, path, heads, rate)
	})
	sc.Step(`^the last ShiftPlanCommitted event is for site "([^"]*)" shift "([^"]*)"$`, w.lastShiftPlanCommittedIs)
	sc.Step(`^the last AssociateCertified event is for associate "([^"]*)" with certification "([^"]*)"$`, w.lastAssociateCertifiedIs)
	sc.Step(`^the last AssociateShiftStarted event is for associate "([^"]*)" with certifications "([^"]*)"$`, w.lastAssociateShiftStartedIs)
}

// registerLabourRequirementRules registers the labour-requirement-copy steps
// (ADR 0043): a LabourPlanCommitted row seeded with an explicit version, so
// scenarios can pin the copy's version guard (decision 1: only a greater
// version is applied; a redelivered or reordered older message never rolls
// the copy back).
func registerLabourRequirementRules(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^warehouse-planning has published a labour plan of (\d+) heads? at rate ([0-9.]+) for site "([^"]*)" shift "([^"]*)" path "([^"]*)" as version (\d+)$`, w.labourPlanPublishedAtVersion)
	sc.Step(`^a late labour plan of (\d+) heads? at rate ([0-9.]+) for site "([^"]*)" shift "([^"]*)" path "([^"]*)" is delivered as version (\d+)$`, w.labourPlanPublishedAtVersion)
}
