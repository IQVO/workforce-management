package associate

import (
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/domain/shared"
)

func TestNewAssociateShift_RaisesStartedEvent(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", []shared.Certification{"pack"}, now)

	events := a.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].EventName() != "AssociateShiftStarted" {
		t.Fatalf("expected AssociateShiftStarted, got %s", events[0].EventName())
	}
	if !a.HasCertification("pack") {
		t.Fatal("expected associate to hold initial certification")
	}
}

func TestCertify_AddsCertificationAndRaisesEvent(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)
	a.PullEvents()

	if err := a.Certify("hazmat", now); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !a.HasCertification("hazmat") {
		t.Fatal("expected certification to be added")
	}
	events := a.PullEvents()
	if len(events) != 1 || events[0].EventName() != "AssociateCertified" {
		t.Fatalf("expected AssociateCertified event, got %+v", events)
	}
}

func TestStartBreak_RejectsDoubleStart(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)

	if err := a.StartBreak(now); err != nil {
		t.Fatalf("unexpected error starting break: %v", err)
	}
	if err := a.StartBreak(now); err != ErrAlreadyOnBreak {
		t.Fatalf("expected ErrAlreadyOnBreak, got %v", err)
	}
}

func TestEndBreak_RejectsWhenNotOnBreak(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)

	if err := a.EndBreak(now); err != ErrNotOnBreak {
		t.Fatalf("expected ErrNotOnBreak, got %v", err)
	}
}

// TestCanBeAssigned_RejectsWhileOnBreak is a Definition-of-Done named
// failing-path test: assignment while on an active break must be rejected.
func TestCanBeAssigned_RejectsWhileOnBreak(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", []shared.Certification{"pack"}, now)
	if err := a.StartBreak(now); err != nil {
		t.Fatalf("unexpected error starting break: %v", err)
	}

	if err := a.CanBeAssigned(); err != ErrOnBreak {
		t.Fatalf("expected ErrOnBreak, got %v", err)
	}
}

func TestCanBeAssigned_AllowsWhenNotOnBreak(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", []shared.Certification{"pack"}, now)

	if err := a.CanBeAssigned(); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

// TestLogHours_RejectsExceedingMaxHoursPerShift is a Definition-of-Done named
// failing-path test: hours logged must not exceed a configured max.
func TestLogHours_RejectsExceedingMaxHoursPerShift(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)
	const maxHours = 8.0

	if err := a.LogHours(6, maxHours); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := a.LogHours(3, maxHours); err != ErrMaxHoursExceeded {
		t.Fatalf("expected ErrMaxHoursExceeded, got %v", err)
	}
	if a.HoursLogged() != 6 {
		t.Fatalf("expected hours logged to remain 6 after rejected log, got %v", a.HoursLogged())
	}
}

func TestLogHours_AllowsUpToMax(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)
	const maxHours = 8.0

	if err := a.LogHours(8, maxHours); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.HoursLogged() != 8 {
		t.Fatalf("expected 8 hours logged, got %v", a.HoursLogged())
	}
}

func TestRehydrate_ReflectsPersistedStateAndRaisesNoEvents(t *testing.T) {
	certs := []shared.Certification{"pack", "hazmat"}
	a := Rehydrate("assoc-1", certs, true, 4.5, false, 3)

	if a.AssociateId() != "assoc-1" {
		t.Fatalf("expected AssociateId assoc-1, got %v", a.AssociateId())
	}
	if !a.IsOnBreak() {
		t.Fatal("expected IsOnBreak to be true")
	}
	if a.HoursLogged() != 4.5 {
		t.Fatalf("expected HoursLogged 4.5, got %v", a.HoursLogged())
	}
	if a.Ended() {
		t.Fatal("expected Ended to be false")
	}
	if a.Version() != 3 {
		t.Fatalf("expected Rehydrate to preserve version 3, got %d", a.Version())
	}

	got := make(map[shared.Certification]struct{}, len(certs))
	for _, c := range a.Certifications() {
		got[c] = struct{}{}
	}
	want := map[shared.Certification]struct{}{"pack": {}, "hazmat": {}}
	if len(got) != len(want) {
		t.Fatalf("expected %d certifications, got %d (%+v)", len(want), len(got), got)
	}
	for c := range want {
		if _, ok := got[c]; !ok {
			t.Fatalf("expected certification %v to be present, got %+v", c, got)
		}
	}

	if events := a.PullEvents(); len(events) != 0 {
		t.Fatalf("expected Rehydrate to raise no events, got %+v", events)
	}
}

func TestRehydrate_EndedTrueReflectedByEnded(t *testing.T) {
	a := Rehydrate("assoc-1", nil, false, 0, true, 1)

	if !a.Ended() {
		t.Fatal("expected Ended to be true")
	}
	if a.IsOnBreak() {
		t.Fatal("expected IsOnBreak to be false")
	}
}

func TestCertifications_ReturnsAllHeldCertifications(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", []shared.Certification{"pack", "pick"}, now)
	a.PullEvents()
	if err := a.Certify("hazmat", now); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := make(map[shared.Certification]struct{})
	for _, c := range a.Certifications() {
		got[c] = struct{}{}
	}
	want := map[shared.Certification]struct{}{"pack": {}, "pick": {}, "hazmat": {}}
	if len(got) != len(want) {
		t.Fatalf("expected %d certifications, got %d (%+v)", len(want), len(got), got)
	}
	for c := range want {
		if _, ok := got[c]; !ok {
			t.Fatalf("expected certification %v to be present, got %+v", c, got)
		}
	}
}

func TestAssociateId_ReturnsIdentity(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-42", nil, now)

	if a.AssociateId() != "assoc-42" {
		t.Fatalf("expected assoc-42, got %v", a.AssociateId())
	}
}

func TestIsOnBreak_ReflectsBreakState(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)

	if a.IsOnBreak() {
		t.Fatal("expected IsOnBreak to be false before any break")
	}
	if err := a.StartBreak(now); err != nil {
		t.Fatalf("unexpected error starting break: %v", err)
	}
	if !a.IsOnBreak() {
		t.Fatal("expected IsOnBreak to be true after StartBreak")
	}
}

func TestEnded_ReflectsShiftState(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)

	if a.Ended() {
		t.Fatal("expected Ended to be false before EndShift")
	}
	a.EndShift(now)
	if !a.Ended() {
		t.Fatal("expected Ended to be true after EndShift")
	}
}

func TestEndBreak_SucceedsAndRaisesEvent(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)
	if err := a.StartBreak(now); err != nil {
		t.Fatalf("unexpected error starting break: %v", err)
	}
	a.PullEvents()

	if err := a.EndBreak(now); err != nil {
		t.Fatalf("unexpected error ending break: %v", err)
	}
	if a.IsOnBreak() {
		t.Fatal("expected IsOnBreak to be false after EndBreak")
	}
	events := a.PullEvents()
	if len(events) != 1 || events[0].EventName() != "AssociateBreakEnded" {
		t.Fatalf("expected AssociateBreakEnded event, got %+v", events)
	}
}

func TestEndBreak_RejectsAfterShiftEnded(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)
	if err := a.StartBreak(now); err != nil {
		t.Fatalf("unexpected error starting break: %v", err)
	}
	a.EndShift(now)

	if err := a.EndBreak(now); err != ErrShiftEnded {
		t.Fatalf("expected ErrShiftEnded, got %v", err)
	}
}

func TestLogHours_RejectsAfterShiftEnded(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)
	const maxHours = 8.0
	if err := a.LogHours(2, maxHours); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a.EndShift(now)

	if err := a.LogHours(1, maxHours); err != ErrShiftEnded {
		t.Fatalf("expected ErrShiftEnded, got %v", err)
	}
	if a.HoursLogged() != 2 {
		t.Fatalf("expected hours logged to remain 2 after rejected log, got %v", a.HoursLogged())
	}
}

func TestEndShift_RaisesEventOnceAndBlocksFurtherOps(t *testing.T) {
	now := time.Now()
	a := NewAssociateShift("assoc-1", nil, now)
	a.PullEvents()

	a.EndShift(now)
	events := a.PullEvents()
	if len(events) != 1 || events[0].EventName() != "AssociateShiftEnded" {
		t.Fatalf("expected AssociateShiftEnded event, got %+v", events)
	}

	a.EndShift(now)
	if events := a.PullEvents(); len(events) != 0 {
		t.Fatalf("expected no event on double end, got %+v", events)
	}

	if err := a.Certify("pack", now); err != ErrShiftEnded {
		t.Fatalf("expected ErrShiftEnded, got %v", err)
	}
	if err := a.StartBreak(now); err != ErrShiftEnded {
		t.Fatalf("expected ErrShiftEnded, got %v", err)
	}
	if err := a.CanBeAssigned(); err != ErrShiftEnded {
		t.Fatalf("expected ErrShiftEnded, got %v", err)
	}
}
