package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	inbound "github.com/claudioed/workforce-management/internal/adapters/inbound/http"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
)

func TestHousekeepingSettingsFromEnv_Defaults(t *testing.T) {
	t.Setenv("HOUSEKEEPING_INTERVAL", "")
	t.Setenv("IDEMPOTENCY_KEY_TTL", "")
	t.Setenv("OUTBOX_RETENTION", "")

	got := housekeepingSettingsFromEnv(quietLogger())
	want := housekeepingSettings{
		interval:        postgres.DefaultSweepInterval,
		idempotencyTTL:  24 * time.Hour,
		outboxRetention: 7 * 24 * time.Hour,
	}
	if got != want {
		t.Fatalf("defaults = %+v, want %+v (ADR-0028: 1h sweep, 24h key TTL, 7d outbox retention)", got, want)
	}
}

func TestHousekeepingSettingsFromEnv_Overrides(t *testing.T) {
	t.Setenv("HOUSEKEEPING_INTERVAL", "5m")
	t.Setenv("IDEMPOTENCY_KEY_TTL", "36h")
	t.Setenv("OUTBOX_RETENTION", "72h")

	got := housekeepingSettingsFromEnv(quietLogger())
	want := housekeepingSettings{interval: 5 * time.Minute, idempotencyTTL: 36 * time.Hour, outboxRetention: 72 * time.Hour}
	if got != want {
		t.Fatalf("overrides = %+v, want %+v", got, want)
	}
}

func TestEnvDurationAllowZero(t *testing.T) {
	const key = "ADRFIX_TEST_DURATION"
	def := 3 * time.Hour
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"unset uses default", "", def},
		{"valid", "90s", 90 * time.Second},
		{"zero means disabled and is honored", "0", 0},
		{"garbage falls back to default", "soon", def},
		{"negative falls back to default", "-5m", def},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(key, tt.raw)
			if got := envDurationAllowZero(quietLogger(), key, def); got != tt.want {
				t.Fatalf("envDurationAllowZero(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// HOUSEKEEPING_INTERVAL=0 disables the sweeper: nothing is started (so a nil
// pool is never touched) and the returned stop func is a safe no-op.
func TestStartSweeper_ZeroIntervalDisablesIt(t *testing.T) {
	stop := startSweeper(nil, housekeepingSettings{interval: 0}, quietLogger())
	if stop == nil {
		t.Fatal("stop func must never be nil")
	}
	stop()
	stop() // idempotent
}

func TestShutdownDrainDelayFromEnv(t *testing.T) {
	t.Setenv("SHUTDOWN_DRAIN_DELAY", "")
	if got := shutdownDrainDelayFromEnv(quietLogger()); got != defaultShutdownDrainDelay {
		t.Errorf("default = %v, want %v (two 5s readinessProbe periods)", got, defaultShutdownDrainDelay)
	}
	t.Setenv("SHUTDOWN_DRAIN_DELAY", "0")
	if got := shutdownDrainDelayFromEnv(quietLogger()); got != 0 {
		t.Errorf("SHUTDOWN_DRAIN_DELAY=0 = %v, want 0 (tests disable the wait)", got)
	}
	t.Setenv("SHUTDOWN_DRAIN_DELAY", "3s")
	if got := shutdownDrainDelayFromEnv(quietLogger()); got != 3*time.Second {
		t.Errorf("override = %v, want 3s", got)
	}
}

// ADR-0022 §8: readiness flips FIRST, then a drain delay passes during
// which /readyz already answers 503 and the listener is still up, and only
// then does the HTTP server shut down; consumers, the sweeper and the relay
// stop after it. Before this was fixed the listener closed in the same
// instant as the flip, so a readinessProbe (period 5s) never saw it.
func TestShutdownSequence_OrderAndDrainWindow(t *testing.T) {
	readiness := &inbound.Readiness{}

	const drain = 80 * time.Millisecond
	var (
		mu     sync.Mutex
		order  []string
		flipAt time.Time
	)
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}

	var readyDuringDrain bool
	seq := shutdownSequence{
		logger:     quietLogger(),
		readiness:  readiness,
		drainDelay: drain,
		timeout:    time.Second,
		httpShutdown: func(context.Context) error {
			record("http")
			if readiness.Ready() {
				t.Error("readiness must already be not-ready when the HTTP server shuts down")
			}
			if elapsed := time.Since(flipAt); elapsed < drain {
				t.Errorf("HTTP shutdown ran %v after the flip, want >= the %v drain delay", elapsed, drain)
			}
			return nil
		},
		stopConsumers: func() { record("consumers") },
		stopSweeper:   func() { record("sweeper") },
		stopRelay:     func(context.Context) { record("relay") },
	}

	// Observe readiness from a concurrent "kubelet" during the drain window.
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		deadline := time.Now().Add(drain)
		for time.Now().Before(deadline) {
			if !readiness.Ready() {
				readyDuringDrain = true
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	flipAt = time.Now()
	if err := seq.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	<-probeDone

	if !readyDuringDrain {
		t.Error("a readiness probe never observed /readyz = not-ready during the drain window")
	}
	want := []string{"http", "consumers", "sweeper", "relay"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// With a zero drain delay (tests) the sequence does not wait but keeps the
// same order, and a failing HTTP shutdown is reported without skipping the
// remaining steps.
func TestShutdownSequence_ZeroDrainAndErrorStillStopsEverything(t *testing.T) {
	readiness := &inbound.Readiness{}
	var stopped []string
	wantErr := http.ErrServerClosed
	seq := shutdownSequence{
		logger:        quietLogger(),
		readiness:     readiness,
		drainDelay:    0,
		timeout:       time.Second,
		httpShutdown:  func(context.Context) error { return wantErr },
		stopConsumers: func() { stopped = append(stopped, "consumers") },
		stopSweeper:   func() { stopped = append(stopped, "sweeper") },
		stopRelay:     func(context.Context) { stopped = append(stopped, "relay") },
	}
	start := time.Now()
	if err := seq.run(); err != wantErr {
		t.Fatalf("run = %v, want the HTTP shutdown error", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("a zero drain delay must not wait")
	}
	if len(stopped) != 3 {
		t.Fatalf("stopped = %v, want consumers, sweeper and relay still stopped", stopped)
	}
	if readiness.Ready() {
		t.Error("readiness must be not-ready after shutdown")
	}
}

// End to end against a real http.Server: /readyz answers 503 while the
// listener is still accepting during the drain window.
func TestShutdownSequence_ReadyzIs503WhileListenerStillServes(t *testing.T) {
	readiness := &inbound.Readiness{}
	router := inbound.NewRouter(&inbound.Handler{Readiness: readiness}, quietLogger(), "")
	ts := httptest.NewServer(router)
	defer ts.Close()

	status := make(chan int, 1)
	seq := shutdownSequence{
		logger:     quietLogger(),
		readiness:  readiness,
		drainDelay: 100 * time.Millisecond,
		timeout:    time.Second,
		httpShutdown: func(context.Context) error {
			return nil
		},
		stopConsumers: func() {},
		stopSweeper:   func() {},
		stopRelay:     func(context.Context) {},
	}
	go func() {
		time.Sleep(40 * time.Millisecond) // inside the drain window
		resp, err := http.Get(ts.URL + "/readyz")
		if err != nil {
			status <- -1
			return
		}
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	if err := seq.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := <-status; got != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz during the drain window = %d, want 503 with the listener still up", got)
	}
}
