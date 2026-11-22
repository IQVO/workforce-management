package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// fakeTx records what runFreshRequest does to its transaction and savepoint,
// so the "which statuses are stored" decision is unit-tested without Docker
// (the real-Postgres behaviour is idempotency_5xx_integration_test.go).
type fakeTx struct {
	pgx.Tx
	sp       *fakeTx // the savepoint handed out by Begin
	execs    []string
	commits  int
	rollback int
}

func (f *fakeTx) Begin(context.Context) (pgx.Tx, error) {
	f.sp = &fakeTx{}
	return f.sp, nil
}

func (f *fakeTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	return pgconn.CommandTag{}, nil
}

func (f *fakeTx) Commit(context.Context) error   { f.commits++; return nil }
func (f *fakeTx) Rollback(context.Context) error { f.rollback++; return nil }

func (f *fakeTx) stored() bool {
	for _, s := range f.execs {
		if strings.Contains(s, "UPDATE idempotency_keys") {
			return true
		}
	}
	return false
}

func runFresh(t *testing.T, status int) (*httptest.ResponseRecorder, *fakeTx) {
	t.Helper()
	tx := &fakeTx{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Probe", "kept")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("body-" + http.StatusText(status)))
	})
	rec := httptest.NewRecorder()
	runFreshRequest(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`)), context.Background(), tx, "K", next)
	return rec, tx
}

func TestRunFreshRequest_5xxIsNeverStored(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504, 599} {
		rec, tx := runFresh(t, status)
		if tx.stored() || tx.commits != 0 {
			t.Errorf("%d: stored=%v commits=%d, want nothing stored and no commit", status, tx.stored(), tx.commits)
		}
		if tx.rollback != 1 {
			t.Errorf("%d: transaction rollbacks = %d, want exactly 1 (key row and handler writes undone)", status, tx.rollback)
		}
		if tx.sp.commits != 0 {
			t.Errorf("%d: the savepoint was released; the handler's writes must not survive", status)
		}
		if rec.Code != status || rec.Body.String() != "body-"+http.StatusText(status) || rec.Header().Get("X-Probe") != "kept" {
			t.Errorf("%d: client saw %d %q %q, want the handler's own answer written verbatim", status, rec.Code, rec.Body.String(), rec.Header().Get("X-Probe"))
		}
	}
}

// runFreshClassed drives runFreshRequest with a handler that answers err
// through the PRODUCTION error writer (writeError), so the catalogue's class
// mark travels on the capture recorder exactly as in production. status is
// redundant with statusFor(err) but asserted for readability.
func runFreshClassed(t *testing.T, err error) (*httptest.ResponseRecorder, *fakeTx) {
	t.Helper()
	tx := &fakeTx{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, statusFor(err), err)
	})
	rec := httptest.NewRecorder()
	runFreshRequest(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`)), context.Background(), tx, "K", next)
	return rec, tx
}

// A FINAL refusal (a function of the request alone) is stored and committed
// with the key row; its savepoint is rolled back (a refusal's writes never
// commit). The client still sees the exact answer.
func TestRunFreshRequest_FinalRefusalIsStored(t *testing.T) {
	rec, tx := runFreshClassed(t, shared.ErrEmptyPathId)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("client saw %d, want 400", rec.Code)
	}
	if !tx.stored() || tx.commits != 1 || tx.rollback != 0 {
		t.Errorf("stored=%v commits=%d rollbacks=%d, want stored and committed", tx.stored(), tx.commits, tx.rollback)
	}
	if tx.sp.rollback != 1 || tx.sp.commits != 0 {
		t.Errorf("savepoint commits=%d rollbacks=%d, want 0/1: a FINAL refusal's writes vanish", tx.sp.commits, tx.sp.rollback)
	}
}

// A RETRYABLE refusal (state that can change, e.g. an optimistic-lock loss)
// is released by exactly the branch that releases a 5xx: the whole
// transaction rolls back — key row and writes — nothing is stored, and the
// client sees the answer verbatim.
func TestRunFreshRequest_RetryableRefusalIsReleased(t *testing.T) {
	for _, err := range []error{ports.ErrConcurrentModification, ports.ErrInstalledCapacityUnavailable, ports.ErrNotFound} {
		rec, tx := runFreshClassed(t, err)
		if tx.stored() || tx.commits != 0 {
			t.Errorf("%v: stored=%v commits=%d, want nothing stored and no commit", err, tx.stored(), tx.commits)
		}
		if tx.rollback != 1 {
			t.Errorf("%v: transaction rollbacks = %d, want exactly 1", err, tx.rollback)
		}
		if tx.sp != nil && (tx.sp.commits != 0 || tx.sp.rollback != 0) {
			t.Errorf("%v: savepoint was resolved, want the whole transaction rolled back first", err)
		}
		if rec.Code != statusFor(err) {
			t.Errorf("%v: client saw %d, want %d", err, rec.Code, statusFor(err))
		}
	}
}

// An OUTCOME refusal would commit the command's writes together with the
// stored answer (savepoint RELEASED, then the key row commit). No catalogue
// entry of this service uses the class — this store cannot produce a refusal
// that committed anything — but the mechanism is proven so the first OUTCOME
// entry is a catalogue change, not a middleware change.
func TestRunFreshRequest_OutcomeMechanismCommitsWritesWithTheAnswer(t *testing.T) {
	tx := &fakeTx{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"type":".../outcome-probe"}`))
		// what writeError would do for an OUTCOME-classed catalogue entry:
		if cap, ok := w.(*captureRecorder); ok {
			cap.markedClass = replayOutcome
		}
	})
	rec := httptest.NewRecorder()
	runFreshRequest(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`)), context.Background(), tx, "K", next)
	if !tx.stored() || tx.commits != 1 {
		t.Fatalf("stored=%v commits=%d, want the refusal stored in the same commit as the writes", tx.stored(), tx.commits)
	}
	if tx.sp.commits != 1 || tx.sp.rollback != 0 {
		t.Errorf("savepoint commits=%d rollbacks=%d, want 1/0: the command's writes must commit with the answer", tx.sp.commits, tx.sp.rollback)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("client saw %d, want 409", rec.Code)
	}
}

// A refusal that arrives UNMARKED (a handler writing a problem by hand,
// bypassing the shared error writers) is released like RETRYABLE: the safe
// fallback — never a silently-stored refusal.
func TestRunFreshRequest_UnmarkedRefusalIsReleasedAsTheSafeFallback(t *testing.T) {
	for _, status := range []int{400, 409, 422, 499} {
		rec, tx := runFresh(t, status)
		if tx.stored() || tx.commits != 0 || tx.rollback != 1 {
			t.Errorf("%d: stored=%v commits=%d rollbacks=%d, want released (nothing stored, one rollback)", status, tx.stored(), tx.commits, tx.rollback)
		}
		if rec.Code != status {
			t.Errorf("%d: client saw %d", status, rec.Code)
		}
	}
}

// A 2xx is stored and committed with its writes (savepoint released).
func TestRunFreshRequest_2xxIsStored(t *testing.T) {
	for _, status := range []int{200, 201, 204} {
		rec, tx := runFresh(t, status)
		if !tx.stored() || tx.commits != 1 || tx.rollback != 0 {
			t.Errorf("%d: stored=%v commits=%d rollbacks=%d, want stored and committed", status, tx.stored(), tx.commits, tx.rollback)
		}
		if tx.sp.commits != 1 || tx.sp.rollback != 0 {
			t.Errorf("%d: savepoint commits=%d rollbacks=%d, want 1/0", status, tx.sp.commits, tx.sp.rollback)
		}
		if rec.Code != status {
			t.Errorf("%d: client saw %d", status, rec.Code)
		}
	}
}

func TestRunFreshRequest_PanicRollsBackAndRePanics(t *testing.T) {
	tx := &fakeTx{}
	defer func() {
		if p := recover(); p != "boom" || tx.rollback != 1 || tx.stored() || tx.commits != 0 {
			t.Fatalf("recovered=%v rollbacks=%d stored=%v commits=%d; want the panic re-raised after one rollback, nothing stored", p, tx.rollback, tx.stored(), tx.commits)
		}
	}()
	runFreshRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil), context.Background(), tx, "K",
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
}
