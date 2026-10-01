package fulfillmentexecution_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claudioed/workforce-management/internal/adapters/outbound/fulfillmentexecution"
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
// documented "no translation table" contract.
const (
	gobreakerClosed = 0
	gobreakerOpen   = 2
)

// countingErrDoer always fails with err, counting calls so a test can
// prove the breaker stopped calling the doer at all once open.
type countingErrDoer struct {
	err   error
	calls int32
}

func (d *countingErrDoer) Do(*http.Request) (*http.Response, error) {
	atomic.AddInt32(&d.calls, 1)
	return nil, d.err
}

// TestBreakerClient_OpensAfterConsecutiveFailures_ShortCircuitsToFallback
// is the ADR-0022 acceptance test: a fake HTTPDoer that always fails
// drives 5 consecutive InstalledCapacity failures
// (resilience.ReadyToTrip's ConsecutiveFailures>=5 leg), the breaker
// opens, and every call after that is short-circuited to
// PermissiveClient's EXISTING fail-loud behaviour
// (ports.ErrInstalledCapacityUnavailable) WITHOUT ever reaching the
// fake doer again.
func TestBreakerClient_OpensAfterConsecutiveFailures_ShortCircuitsToFallback(t *testing.T) {
	boom := errors.New("connection refused")
	fake := &countingErrDoer{err: boom}
	inner := fulfillmentexecution.NewClient("http://fulfillment-execution.local", fake)
	recorder := &recordingRecorder{}
	client := fulfillmentexecution.NewBreakerClient(inner, recorder)

	for i := 0; i < 5; i++ {
		_, err := client.InstalledCapacity(context.Background(), shared.Capability("pack"))
		if !errors.Is(err, ports.ErrInstalledCapacityUnavailable) {
			t.Fatalf("call %d: err = %v, want ErrInstalledCapacityUnavailable (breaker still closed)", i, err)
		}
	}
	if recorder.last() != gobreakerOpen {
		t.Fatalf("breaker state after 5 consecutive failures = %d, want open (%d)", recorder.last(), gobreakerOpen)
	}
	callsBeforeShortCircuit := atomic.LoadInt32(&fake.calls)

	_, err := client.InstalledCapacity(context.Background(), shared.Capability("pack"))
	if !errors.Is(err, ports.ErrInstalledCapacityUnavailable) {
		t.Fatalf("while open, err = %v, want %v (the existing permissive fail-loud behaviour)", err, ports.ErrInstalledCapacityUnavailable)
	}
	if atomic.LoadInt32(&fake.calls) != callsBeforeShortCircuit {
		t.Fatal("doer was called again while the breaker is open -- it must short-circuit instead")
	}
}

// flakyThenOKDoer fails the first failCount calls with err, then
// delegates to ok -- the "fake HTTPDoer that fails N times then
// succeeds" pattern, used here to drive the half-open probe recovery.
type flakyThenOKDoer struct {
	remaining int32
	err       error
	ok        interface {
		Do(*http.Request) (*http.Response, error)
	}
}

func (d *flakyThenOKDoer) Do(req *http.Request) (*http.Response, error) {
	if atomic.AddInt32(&d.remaining, -1) >= 0 {
		return nil, d.err
	}
	return d.ok.Do(req)
}

// TestBreakerClient_HalfOpenProbeRecoversToClosed proves the full
// state-machine round trip: open -> (cooldown) -> half-open probe
// succeeds -> closed, and that the successful probe is served by the
// REAL upstream (not the fallback) once recovered.
func TestBreakerClient_HalfOpenProbeRecoversToClosed(t *testing.T) {
	var got captured
	srv := newServer(t, http.StatusOK, `{"capability":"pack","installed":4}`, &got)
	realDoer := http.DefaultClient
	fake := &flakyThenOKDoer{remaining: 5, err: errors.New("connection refused"), ok: realDoer}
	inner := fulfillmentexecution.NewClient(srv.URL, fake)

	recorder := &recordingRecorder{}
	// A short breaker cooldown so the test does not sleep for
	// resilience.DefaultTimeout (30s) in real time.
	client := fulfillmentexecution.NewBreakerClientWithTimeout(inner, recorder, 100*time.Millisecond)

	for i := 0; i < 5; i++ {
		if _, err := client.InstalledCapacity(context.Background(), shared.Capability("pack")); err == nil {
			t.Fatalf("call %d unexpectedly succeeded before the fake doer's failure budget was exhausted", i)
		}
	}
	if recorder.last() != gobreakerOpen {
		t.Fatalf("breaker state after 5 failures = %d, want open (%d)", recorder.last(), gobreakerOpen)
	}

	// Wait out the cooldown so the breaker allows a half-open probe.
	time.Sleep(150 * time.Millisecond)

	installed, err := client.InstalledCapacity(context.Background(), shared.Capability("pack"))
	if err != nil {
		t.Fatalf("half-open probe: InstalledCapacity: %v", err)
	}
	if installed != 4 {
		t.Fatalf("installed = %d, want 4", installed)
	}
	if recorder.last() != gobreakerClosed {
		t.Fatalf("breaker state after successful half-open probe = %d, want closed (%d)", recorder.last(), gobreakerClosed)
	}
}

// TestBreakerClient_NeverRetries proves this client makes exactly ONE
// HTTP call per InstalledCapacity invocation -- unlike
// laborperformance.BreakerClient, this dependency gates a commit that
// mutates real state and deliberately gets no blind retry (see the
// package doc comment).
func TestBreakerClient_NeverRetries(t *testing.T) {
	boom := errors.New("connection refused")
	fake := &countingErrDoer{err: boom}
	inner := fulfillmentexecution.NewClient("http://fulfillment-execution.local", fake)
	client := fulfillmentexecution.NewBreakerClient(inner, nil)

	_, err := client.InstalledCapacity(context.Background(), shared.Capability("pack"))
	if !errors.Is(err, ports.ErrInstalledCapacityUnavailable) {
		t.Fatalf("err = %v, want ErrInstalledCapacityUnavailable", err)
	}
	if got := atomic.LoadInt32(&fake.calls); got != 1 {
		t.Fatalf("doer calls = %d, want exactly 1 -- this client must never retry", got)
	}
}
