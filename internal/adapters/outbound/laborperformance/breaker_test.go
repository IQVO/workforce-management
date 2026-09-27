package laborperformance_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/laborperformance"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// recordingRecorder implements resilience.StateRecorder, capturing every
// state transition in order so a test can assert the breaker actually
// opened/closed at the expected point, not just that the call outcomes
// looked right.
type recordingRecorder struct {
	states []int64
}

func (r *recordingRecorder) SetState(_ string, state int64) {
	r.states = append(r.states, state)
}

func (r *recordingRecorder) last() int64 {
	if len(r.states) == 0 {
		return -1
	}
	return r.states[len(r.states)-1]
}

// The gobreaker.State values (0=closed,1=half-open,2=open) are
// duplicated here as untyped constants rather than importing gobreaker
// into this _test package, matching resilience.RecordStateChange's own
// documented "no translation table" contract -- this test asserts
// against that SAME numbering, not a re-derived one.
const (
	gobreakerClosed = 0
	gobreakerOpen   = 2
)

// countingFakeDoer wraps fakeDoer-shaped behaviour with a call counter,
// so a test can prove retry attempt counts and breaker short-circuiting
// precisely.
type countingFakeDoer struct {
	resp  func() *http.Response
	err   error
	calls int32
}

func (d *countingFakeDoer) Do(*http.Request) (*http.Response, error) {
	atomic.AddInt32(&d.calls, 1)
	if d.err != nil {
		return nil, d.err
	}
	return d.resp(), nil
}

func jsonResponse(status int, body string) *http.Response {
	resp := &http.Response{StatusCode: status, Header: http.Header{}, Body: http.NoBody}
	resp.Header.Set("Content-Type", "application/json")
	if body != "" {
		resp.Body = io.NopCloser(strings.NewReader(body))
	}
	return resp
}

// TestBreakerClient_RetriesTransportErrorsUpToMaxAttempts is the
// ADR-0022 retry acceptance test: a fake HTTPDoer that always fails is
// retried exactly maxRetryAttempts (3) times per MeanActualSeconds call
// -- not fewer (leaving retry budget on the table) and not more (an
// unbounded retry storm).
func TestBreakerClient_RetriesTransportErrorsUpToMaxAttempts(t *testing.T) {
	boom := errors.New("connection refused")
	doer := &countingFakeDoer{err: boom}
	inner := laborperformance.NewClient("http://labor-performance.local", doer)
	client := laborperformance.NewBreakerClient(inner, nil)

	_, err := client.MeanActualSeconds(context.Background(), shared.PathId("pack"))
	if !errors.Is(err, ports.ErrMeasuredRateUnavailable) {
		t.Fatalf("MeanActualSeconds must fail with ErrMeasuredRateUnavailable even after exhausting retries, got %v", err)
	}
	if got := atomic.LoadInt32(&doer.calls); got != 3 {
		t.Fatalf("doer calls = %d, want exactly 3 (1 original + 2 retries)", got)
	}
}

// TestBreakerClient_SucceedsOnSecondAttempt proves a transient failure
// (1 failure then success) is transparently retried into a successful
// result, with no error and no fail-open fallback.
func TestBreakerClient_SucceedsOnSecondAttempt(t *testing.T) {
	var call int32
	doer := &countingFakeDoer{
		resp: func() *http.Response {
			if atomic.AddInt32(&call, 1) == 1 {
				return jsonResponse(http.StatusInternalServerError, "")
			}
			return jsonResponse(http.StatusOK, `{"taskType":"PACK","meanActualSeconds":42.0}`)
		},
	}
	inner := laborperformance.NewClient("http://labor-performance.local", doer)
	client := laborperformance.NewBreakerClient(inner, nil)

	seconds, err := client.MeanActualSeconds(context.Background(), shared.PathId("pack"))
	if err != nil {
		t.Fatalf("MeanActualSeconds: %v", err)
	}
	if seconds != 42.0 {
		t.Fatalf("seconds = %v, want 42.0", seconds)
	}
	if got := atomic.LoadInt32(&doer.calls); got != 2 {
		t.Fatalf("doer calls = %d, want exactly 2 (1 failure + 1 success)", got)
	}
}

// TestBreakerClient_PathWithNoTaskType_NeverCallsDoer proves a pathId
// with no labor-performance TaskType counterpart fails fast (no HTTP
// call, no retry budget spent) -- mirroring Client's own
// TestMeanActualSecondsRejectsPathsWithNoTaskType contract exactly.
func TestBreakerClient_PathWithNoTaskType_NeverCallsDoer(t *testing.T) {
	doer := &countingFakeDoer{err: errors.New("should never be called")}
	inner := laborperformance.NewClient("http://labor-performance.local", doer)
	client := laborperformance.NewBreakerClient(inner, nil)

	_, err := client.MeanActualSeconds(context.Background(), shared.PathId("stow"))
	if !errors.Is(err, ports.ErrMeasuredRateUnavailable) {
		t.Fatalf("err = %v, want ErrMeasuredRateUnavailable", err)
	}
	if got := atomic.LoadInt32(&doer.calls); got != 0 {
		t.Fatalf("doer calls = %d, want 0 -- a path with no TaskType must never reach the HTTP call", got)
	}
}

// TestBreakerClient_OpensAfterConsecutiveFailures_ShortCircuitsToFallback
// mirrors order-management's productclassification breaker test: enough
// failed MeanActualSeconds calls (each internally already retried 3x,
// so the breaker sees ONE failure per call, not three) trips the
// breaker, and once open, calls short-circuit to PermissiveClient's
// existing fail-open contract WITHOUT reaching the doer again.
func TestBreakerClient_OpensAfterConsecutiveFailures_ShortCircuitsToFallback(t *testing.T) {
	boom := errors.New("connection refused")
	doer := &countingFakeDoer{err: boom}
	inner := laborperformance.NewClient("http://labor-performance.local", doer)
	recorder := &recordingRecorder{}
	client := laborperformance.NewBreakerClient(inner, recorder)

	// 5 consecutive failed calls (each internally retried 3x) trips
	// the breaker -- ReadyToTrip only sees breaker-level Execute
	// outcomes, i.e. 5, not 15.
	for i := 0; i < 5; i++ {
		_, err := client.MeanActualSeconds(context.Background(), shared.PathId("pack"))
		if !errors.Is(err, ports.ErrMeasuredRateUnavailable) {
			t.Fatalf("call %d: err = %v, want ErrMeasuredRateUnavailable (breaker still closed, exhausted retries)", i, err)
		}
	}
	if recorder.last() != gobreakerOpen {
		t.Fatalf("breaker state after 5 failed calls = %d, want open (%d)", recorder.last(), gobreakerOpen)
	}
	callsBeforeShortCircuit := atomic.LoadInt32(&doer.calls)

	_, err := client.MeanActualSeconds(context.Background(), shared.PathId("pack"))
	if !errors.Is(err, ports.ErrMeasuredRateUnavailable) {
		t.Fatalf("while open, err = %v, want ErrMeasuredRateUnavailable (the existing permissive fail-open behaviour)", err)
	}
	if atomic.LoadInt32(&doer.calls) != callsBeforeShortCircuit {
		t.Fatal("doer was called again while the breaker is open -- it must short-circuit instead")
	}
}

// TestBreakerClient_HalfOpenProbeRecoversToClosed proves the full
// state-machine round trip: open -> (cooldown) -> half-open probe
// succeeds -> closed, and that the successful probe is served by the
// REAL upstream (not the fallback) once recovered.
func TestBreakerClient_HalfOpenProbeRecoversToClosed(t *testing.T) {
	// Each of the 5 outer calls that trips the breaker internally
	// retries up to maxRetryAttempts (3) times before the breaker
	// records ONE failure -- so the failure budget must cover 5*3
	// attempts, not 5, before the doer starts succeeding.
	var remaining int32 = 15
	boom := errors.New("connection refused")
	doer := &countingFakeDoer{
		resp: func() *http.Response {
			return jsonResponse(http.StatusOK, `{"taskType":"PACK","meanActualSeconds":42.0}`)
		},
	}
	flaky := &flakyThenOKDoer{remaining: &remaining, err: boom, ok: doer}
	inner := laborperformance.NewClient("http://labor-performance.local", flaky)

	recorder := &recordingRecorder{}
	// A short breaker cooldown so the test does not sleep for
	// resilience.DefaultTimeout (30s) in real time.
	client := laborperformance.NewBreakerClientWithTimeout(inner, recorder, 100*time.Millisecond)

	for i := 0; i < 5; i++ {
		if _, err := client.MeanActualSeconds(context.Background(), shared.PathId("pack")); err == nil {
			t.Fatalf("call %d unexpectedly succeeded before the fake doer's failure budget was exhausted", i)
		}
	}
	if recorder.last() != gobreakerOpen {
		t.Fatalf("breaker state after 5 failures = %d, want open (%d)", recorder.last(), gobreakerOpen)
	}

	// Wait out the cooldown so the breaker allows a half-open probe.
	time.Sleep(150 * time.Millisecond)

	seconds, err := client.MeanActualSeconds(context.Background(), shared.PathId("pack"))
	if err != nil {
		t.Fatalf("half-open probe: MeanActualSeconds: %v", err)
	}
	if seconds != 42.0 {
		t.Fatalf("seconds = %v, want 42.0", seconds)
	}
	if recorder.last() != gobreakerClosed {
		t.Fatalf("breaker state after successful half-open probe = %d, want closed (%d)", recorder.last(), gobreakerClosed)
	}
}

// flakyThenOKDoer fails every call until remaining reaches 0, then
// delegates to ok -- the "fake HTTPDoer that fails N times then
// succeeds" pattern used to drive the half-open probe recovery.
type flakyThenOKDoer struct {
	remaining *int32
	err       error
	ok        interface {
		Do(*http.Request) (*http.Response, error)
	}
}

func (d *flakyThenOKDoer) Do(req *http.Request) (*http.Response, error) {
	if atomic.AddInt32(d.remaining, -1) >= 0 {
		return nil, d.err
	}
	return d.ok.Do(req)
}
