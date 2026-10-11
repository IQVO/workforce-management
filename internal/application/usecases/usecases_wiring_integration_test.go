//go:build integration

// Package usecases_test proves this context's MAIN write use cases against a
// REAL Postgres (testcontainers): the real Postgres repos, the real
// UnitOfWork, and a buffering LogPublisher, wired exactly like the
// composition roots in cmd/workforce and cmd/mcp. These are integration
// tests in the fleet's sense: they execute the real cross-component
// contracts (version-guarded upserts, the certification/double-booking
// assignment invariants, the site-scoped staffing-gap read, the atomic
// Publish-inside-UoW bracket) against real infrastructure, with no
// in-memory repo fakes anywhere in the path.
//
// The package boots its own throwaway Postgres via testcontainers in
// TestMain: one container for the whole package, migrated once into a
// TEMPLATE database, and each test then gets its own database cloned from
// that template (CREATE DATABASE ... WITH TEMPLATE, a file-level copy:
// milliseconds). Never an external DATABASE_URL, never t.Skip.
package usecases_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/events"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// One Postgres container serves the whole package (containers are slow to
// boot). TestMain starts it, applies every migration ONCE into a template
// database, and each test then gets its own database cloned from that
// template. Isolation is therefore total — no TRUNCATE bookkeeping, no
// dependence on test order, and tests that assert on global state still
// start pristine.
//
// Never an external DATABASE_URL, never t.Skip.
const templateDB = "usecases_migrated_template"

var (
	sharedBaseURL string // connection URL of the container's default database
	dbSeq         atomic.Uint64
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("workforce_usecases"),
		tcpostgres.WithUsername("workforce"),
		tcpostgres.WithPassword("workforce"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		return 1
	}
	defer func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			fmt.Fprintf(os.Stderr, "terminate postgres container: %v\n", err)
		}
	}()

	sharedBaseURL, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres connection string: %v\n", err)
		return 1
	}

	// Migrate a template database once; every test clones it.
	if err := createDatabase(ctx, templateDB); err != nil {
		fmt.Fprintf(os.Stderr, "create template database: %v\n", err)
		return 1
	}
	tmplDB := withDB(sharedBaseURL, templateDB)
	migrations, err := filepath.Abs(filepath.Join("..", "..", "..", "migrations"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve migrations dir: %v\n", err)
		return 1
	}
	if err := postgres.Migrate(tmplDB, migrations); err != nil {
		fmt.Fprintf(os.Stderr, "migrate template: %v\n", err)
		return 1
	}

	return m.Run()
}

// withDB rewrites the path of a connection URL to the named database.
func withDB(baseURL, name string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}

// createDatabase creates an empty database inside the shared container.
func createDatabase(ctx context.Context, name string) error {
	conn, err := pgx.Connect(ctx, sharedBaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

// migratedDB hands the test a connection URL to its own private database,
// cloned from the migrated template. Cloning is a file-level copy, so it
// costs milliseconds and the test's writes never leak into another test.
func migratedDB(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("usecases_%d", dbSeq.Add(1))
	conn, err := pgx.Connect(context.Background(), sharedBaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), fmt.Sprintf(
		"CREATE DATABASE %q WITH TEMPLATE %q", name, templateDB)); err != nil {
		t.Fatalf("clone database: %v", err)
	}
	return withDB(sharedBaseURL, name)
}

// fixedClock is the ports.Clock the use cases already accept; deterministic
// timestamps keep the published events comparable.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

// fleetCatalogue mirrors the fleet's real process-path catalogue
// (process-path-management / warehouse-infra sortable-fc.yaml), the same
// shape every other integration package in this repo uses: UPPER-case
// canonical path ids, each requiring the lower-case capability stations
// are registered with in fulfillment-execution. CommitShiftPlan resolves a
// line's path through it to the capability it queries; AssignLabor
// normalises the required certification through the resolved family's
// MatchPrefix.
func fleetCatalogue() *pathcatalog.Catalogue {
	return pathcatalog.New([]pathcatalog.PathDefinition{
		{Id: "PICK", MatchPrefix: "pick", RequiredCapabilities: []string{"pick"}},
		{Id: "PACK", MatchPrefix: "pack", RequiredCapabilities: []string{"pack"}},
		{Id: "REBIN", MatchPrefix: "rebin", RequiredCapabilities: []string{"rebin"}},
		{Id: "SLAM", MatchPrefix: "slam", RequiredCapabilities: []string{"slam"}},
	})
}

// wired is the real adapter stack over one private migrated database,
// wired exactly like the composition root: real Postgres repos, the real
// UnitOfWork, and a buffering LogPublisher the tests assert on. No
// in-memory repo fakes.
type wired struct {
	associates  *postgres.AssociateRepo
	shiftPlans  *postgres.ShiftPlanRepo
	assignments *postgres.AssignmentRepo
	publisher   *events.LogPublisher
	uow         *postgres.UnitOfWork
	clock       fixedClock
}

// newWiredUsecases opens the pool for a fresh private database and returns
// the repo stack it shares. Tests build their use cases from it, exactly
// like cmd/workforce does.
func newWiredUsecases(t *testing.T) *wired {
	t.Helper()
	ctx := context.Background()
	pool, err := postgres.NewPool(ctx, migratedDB(t))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &wired{
		associates:  postgres.NewAssociateRepo(pool),
		shiftPlans:  postgres.NewShiftPlanRepo(pool),
		assignments: postgres.NewAssignmentRepo(pool),
		publisher:   events.NewLogPublisher(nil),
		uow:         postgres.NewUnitOfWork(pool),
		clock:       fixedClock{time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)},
	}
}

// TestUsecases_AssociateLifecycle_StartAssignReassignEnd drives the
// aggregate lifecycle end-to-end through the real write path: start the
// shift (create), assign labor (state change), reassign to another path
// (second state change), end the shift (close) — asserting the persisted
// state and the published events after each step.
func TestUsecases_AssociateLifecycle_StartAssignReassignEnd(t *testing.T) {
	w := newWiredUsecases(t)
	ctx := context.Background()

	start := &usecases.StartAssociateShift{
		Associates: w.associates, Events: w.publisher, Clock: w.clock, UnitOfWork: w.uow,
	}
	assign := &usecases.AssignLabor{
		Associates: w.associates, Assignments: w.assignments,
		Events: w.publisher, Clock: w.clock, MaxHoursPerShift: 8,
		Catalogue: fleetCatalogue(), UnitOfWork: w.uow,
	}
	end := &usecases.EndAssociateShift{
		Associates: w.associates, Assignments: w.assignments,
		Events: w.publisher, Clock: w.clock, MaxHoursPerShift: 8, UnitOfWork: w.uow,
	}

	// Create: the roster entry starts with the pack certification.
	if _, err := start.ExecuteAtSite(ctx, "ITCOV-A1", []shared.Certification{"pack"}, "WH1"); err != nil {
		t.Fatalf("start shift: %v", err)
	}
	shift, err := w.associates.FindByID(ctx, "ITCOV-A1")
	if err != nil {
		t.Fatalf("reload shift: %v", err)
	}
	if !shift.HasCertification("pack") || shift.Ended() {
		t.Fatalf("shift must be live and pack-certified, got %+v", shift)
	}

	// State change: assign to PACK (catalogue family resolves the required
	// certification "pack").
	if _, err := assign.Execute(ctx, "ITCOV-A1", "PACK"); err != nil {
		t.Fatalf("assign: %v", err)
	}
	la, err := w.assignments.FindByAssociateID(ctx, "ITCOV-A1")
	if err != nil {
		t.Fatalf("reload assignment: %v", err)
	}
	if p, active := la.ActivePathId(); !active || p != "PACK" {
		t.Fatalf("expected an active PACK assignment, got path=%q active=%v", p, active)
	}

	// Second state change: certify the associate for pick, then reassign.
	// Reassigning ends the prior interval, keeps exactly one active
	// assignment (the double-booking invariant is by construction, never
	// by rejecting), and logs the elapsed hours.
	certify := &usecases.CertifyAssociate{
		Associates: w.associates, Events: w.publisher, Clock: w.clock, UnitOfWork: w.uow,
	}
	if err := certify.Execute(ctx, "ITCOV-A1", "pick"); err != nil {
		t.Fatalf("certify: %v", err)
	}
	reassignAt := w.clock.now.Add(2 * time.Hour)
	assign.Clock = fixedClock{reassignAt}
	if _, err := assign.Execute(ctx, "ITCOV-A1", "PICK"); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	la, err = w.assignments.FindByAssociateID(ctx, "ITCOV-A1")
	if err != nil {
		t.Fatalf("reload assignment after reassign: %v", err)
	}
	if p, active := la.ActivePathId(); !active || p != "PICK" {
		t.Fatalf("expected the active assignment moved to PICK, got path=%q active=%v", p, active)
	}
	if len(la.History()) != 1 || la.History()[0].PathId != "PACK" {
		t.Fatalf("expected one closed PACK interval in history, got %+v", la.History())
	}
	shift, err = w.associates.FindByID(ctx, "ITCOV-A1")
	if err != nil {
		t.Fatalf("reload shift after reassign: %v", err)
	}
	if shift.HoursLogged() != 2 {
		t.Fatalf("expected 2 hours logged for the closed interval, got %v", shift.HoursLogged())
	}

	// Close: ending the shift also ends the active assignment.
	endAt := reassignAt.Add(1 * time.Hour)
	end.Clock = fixedClock{endAt}
	if err := end.Execute(ctx, "ITCOV-A1"); err != nil {
		t.Fatalf("end shift: %v", err)
	}
	shift, err = w.associates.FindByID(ctx, "ITCOV-A1")
	if err != nil {
		t.Fatalf("reload shift after end: %v", err)
	}
	if !shift.Ended() {
		t.Fatal("the shift must be persisted as ended")
	}
	la, err = w.assignments.FindByAssociateID(ctx, "ITCOV-A1")
	if err != nil {
		t.Fatalf("reload assignment after end: %v", err)
	}
	if la.IsActive() {
		t.Fatal("ending the shift must end the active assignment")
	}
	if len(la.History()) != 2 {
		t.Fatalf("expected both intervals closed in history, got %+v", la.History())
	}

	// The publisher saw every transition, in order.
	want := []string{
		"AssociateShiftStarted", "LaborAssigned", "AssociateCertified",
		"LaborReassigned", "AssociateShiftEnded",
	}
	got := make([]string, 0, len(want))
	for _, e := range w.publisher.Events() {
		got = append(got, e.EventName())
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("published events %v, want %v", got, want)
	}
}

// TestUsecases_AssignLaborRejectsUncertifiedAssociate proves the domain
// rejection crosses the real Postgres path untouched: the associate exists
// (started through the real repo) but holds no pick certification, so the
// aggregate refuses the assignment and NOTHING is persisted.
func TestUsecases_AssignLaborRejectsUncertifiedAssociate(t *testing.T) {
	w := newWiredUsecases(t)
	ctx := context.Background()

	start := &usecases.StartAssociateShift{
		Associates: w.associates, Events: w.publisher, Clock: w.clock, UnitOfWork: w.uow,
	}
	if _, err := start.Execute(ctx, "ITCOV-A2", []shared.Certification{"pack"}); err != nil {
		t.Fatalf("start shift: %v", err)
	}

	assign := &usecases.AssignLabor{
		Associates: w.associates, Assignments: w.assignments,
		Events: w.publisher, Clock: w.clock, MaxHoursPerShift: 8,
		Catalogue: fleetCatalogue(), UnitOfWork: w.uow,
	}
	if _, err := assign.Execute(ctx, "ITCOV-A2", "PICK"); err == nil {
		t.Fatal("assigning a pack-only associate to PICK must be rejected")
	}
	if _, err := w.assignments.FindByAssociateID(ctx, "ITCOV-A2"); err == nil {
		t.Fatal("a rejected assignment must not be persisted")
	}
	for _, e := range w.publisher.Events() {
		if e.EventName() == "LaborAssigned" || e.EventName() == "LaborReassigned" {
			t.Fatalf("a rejected assignment must publish nothing, saw %s", e.EventName())
		}
	}
}

// TestUsecases_CommitShiftPlanFeedsTheGapReadModel drives the plan
// aggregate lifecycle end-to-end: commit the plan (create), assign one
// associate (state change), read the staffing gap (read model) before and
// after — the gap must flip from understaffed to covered exactly when
// active heads reach planned heads, and the PathUnderstaffed event must be
// published only while the gap is open.
func TestUsecases_CommitShiftPlanFeedsTheGapReadModel(t *testing.T) {
	w := newWiredUsecases(t)
	ctx := context.Background()

	commit := &usecases.CommitShiftPlan{
		ShiftPlans: w.shiftPlans, Events: w.publisher, Clock: w.clock,
		InstalledCapacity: fixedCapacity{"pack": 10}, Catalogue: fleetCatalogue(),
		MaxHoursPerShift: 8, UnitOfWork: w.uow,
	}
	gap := &usecases.GetStaffingGap{
		ShiftPlans: w.shiftPlans, Assignments: w.assignments,
		Events: w.publisher, Clock: w.clock, UnitOfWork: w.uow,
	}
	start := &usecases.StartAssociateShift{
		Associates: w.associates, Events: w.publisher, Clock: w.clock, UnitOfWork: w.uow,
	}
	assign := &usecases.AssignLabor{
		Associates: w.associates, Assignments: w.assignments,
		Events: w.publisher, Clock: w.clock, MaxHoursPerShift: 8,
		Catalogue: fleetCatalogue(), UnitOfWork: w.uow,
	}

	// Create: commit a two-head pack plan for site WH1's shift.
	lines := []shiftplan.PathPlan{{PathId: "PACK", PlannedHeads: 2, PlannedRate: 50, PlannedHours: 8}}
	if _, err := commit.Execute(ctx, "WH1", "S1", lines, map[shared.PathId]int{"PACK": 10}); err != nil {
		t.Fatalf("commit plan: %v", err)
	}
	if _, err := w.shiftPlans.FindByBuildingAndShift(ctx, "WH1", "S1"); err != nil {
		t.Fatalf("reload plan: %v", err)
	}

	// Read model, unstaffed: 0 active heads against 2 planned.
	before, err := gap.ExecuteForSite(ctx, "WH1", "S1", "PACK", "WH1")
	if err != nil {
		t.Fatalf("gap before: %v", err)
	}
	if before.PlannedHeads != 2 || before.ActiveHeads != 0 || !before.Understaffed {
		t.Fatalf("expected 2/0 understaffed before assigning, got %+v", before)
	}

	// State change: one certified associate starts at WH1 and is assigned.
	if _, err := start.ExecuteAtSite(ctx, "ITCOV-B1", []shared.Certification{"pack"}, "WH1"); err != nil {
		t.Fatalf("start shift: %v", err)
	}
	if _, err := assign.Execute(ctx, "ITCOV-B1", "PACK"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// Read model again: 1 active head is still short of 2 planned.
	mid, err := gap.ExecuteForSite(ctx, "WH1", "S1", "PACK", "WH1")
	if err != nil {
		t.Fatalf("gap mid: %v", err)
	}
	if mid.ActiveHeads != 1 || !mid.Understaffed {
		t.Fatalf("expected 2/1 still understaffed, got %+v", mid)
	}

	// A second associate closes the gap: the read model must agree.
	if _, err := start.ExecuteAtSite(ctx, "ITCOV-B2", []shared.Certification{"pack"}, "WH1"); err != nil {
		t.Fatalf("start second shift: %v", err)
	}
	if _, err := assign.Execute(ctx, "ITCOV-B2", "PACK"); err != nil {
		t.Fatalf("assign second: %v", err)
	}
	after, err := gap.ExecuteForSite(ctx, "WH1", "S1", "PACK", "WH1")
	if err != nil {
		t.Fatalf("gap after: %v", err)
	}
	if after.ActiveHeads != 2 || after.Understaffed {
		t.Fatalf("expected 2/2 covered after both assignments, got %+v", after)
	}

	// The publisher saw the plan commit and one PathUnderstaffed per
	// understaffed read (the two reads above), and nothing after coverage.
	var understaffed int
	for _, e := range w.publisher.Events() {
		switch e.EventName() {
		case "ShiftPlanCommitted":
		case "PathUnderstaffed":
			understaffed++
		}
	}
	if understaffed != 2 {
		t.Fatalf("expected exactly 2 PathUnderstaffed events (one per understaffed read), got %d", understaffed)
	}
}

// fixedCapacity scripts installed capacity per station CAPABILITY, the key
// fulfillment-execution actually counts by (a stand-in for the live
// registry, which the composition root fetches over HTTP; the fleet's
// other integration suites script it the same way).
type fixedCapacity map[shared.Capability]int

func (c fixedCapacity) InstalledCapacity(_ context.Context, capability shared.Capability) (int, error) {
	return c[capability], nil
}
