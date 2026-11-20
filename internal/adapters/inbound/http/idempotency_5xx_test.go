package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

func TestRunFreshRequest_2xxAnd4xxStayStored(t *testing.T) {
	cases := []struct {
		status        int
		spCommit, spR int
	}{{200, 1, 0}, {201, 1, 0}, {204, 1, 0}, {400, 0, 1}, {409, 0, 1}, {422, 0, 1}, {499, 0, 1}}
	for _, c := range cases {
		rec, tx := runFresh(t, c.status)
		if !tx.stored() || tx.commits != 1 || tx.rollback != 0 {
			t.Errorf("%d: stored=%v commits=%d rollbacks=%d, want stored and committed", c.status, tx.stored(), tx.commits, tx.rollback)
		}
		if tx.sp.commits != c.spCommit || tx.sp.rollback != c.spR {
			t.Errorf("%d: savepoint commits=%d rollbacks=%d, want %d/%d", c.status, tx.sp.commits, tx.sp.rollback, c.spCommit, c.spR)
		}
		if rec.Code != c.status {
			t.Errorf("%d: client saw %d", c.status, rec.Code)
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
