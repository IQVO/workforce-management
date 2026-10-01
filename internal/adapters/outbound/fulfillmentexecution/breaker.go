// breaker.go wraps Client with a per-dependency circuit breaker
// (sony/gobreaker/v2, ADR-0022): trips on the shared
// resilience.ReadyToTrip condition, and while OPEN falls back to
// PermissiveClient's existing fail-LOUD behaviour
// (ErrInstalledCapacityUnavailable) rather than inventing a new
// fallback path -- an installed-capacity ceiling gates a COMMIT that
// mutates real state, so it must never appear to succeed against a
// tripped breaker any more than against the permissive no-op (see
// permissive.go's doc comment). This is deliberately the ONLY outbound
// client in this service that never retries a call: GET
// /capacity/{capability} feeds CommitShiftPlan's fail-loud validation,
// and this fleet's convention (mirroring order-management's
// inventorystorage.BreakerClient) is that a call gating a mutating
// commit gets a breaker but not blind retry -- see the ADR's
// "retry-only-on-reads-that-are-NOT-a-commit-gate" decision. Unlike
// order-management's inventory-storage client, this IS a pure read
// (GET), but it still gates CommitShiftPlan's real state mutation the
// same way a mutating call would, so it is scoped out of retry for the
// same reason: a retry storm against an already-degraded dependency
// should trip the breaker promptly, not mask the degradation behind
// extra attempts on the exact call that decides whether real capacity
// is being exceeded.
package fulfillmentexecution

import (
	"context"
	"errors"
	"time"

	gobreaker "github.com/sony/gobreaker/v2"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/resilience"
)

// DependencyName labels this breaker's Prometheus gauge series
// (circuit_breaker_state{dependency="fulfillment-execution"}).
const DependencyName = "fulfillment-execution"

// BreakerClient wraps Client with a circuit breaker guarding
// InstalledCapacity.
type BreakerClient struct {
	breaker  *gobreaker.CircuitBreaker[int]
	inner    *Client
	fallback *PermissiveClient
}

var _ ports.InstalledCapacityClient = (*BreakerClient)(nil)

// NewBreakerClient builds a BreakerClient wrapping inner. recorder is
// resilience.StateRecorder (typically
// telemetry.CircuitBreakerMetrics) -- nil is a valid, documented no-op
// (see resilience.RecordStateChange), so a test that does not care
// about the metric never needs to construct one.
func NewBreakerClient(inner *Client, recorder resilience.StateRecorder) *BreakerClient {
	return newBreakerClient(inner, recorder, resilience.DefaultTimeout)
}

// NewBreakerClientWithTimeout is NewBreakerClient with an explicit
// breaker cooldown (gobreaker.Settings.Timeout) instead of
// resilience.DefaultTimeout, so a half-open-recovery test does not have
// to sleep for the full production cooldown in real time. Production
// code should always use NewBreakerClient; this exists for tests.
func NewBreakerClientWithTimeout(inner *Client, recorder resilience.StateRecorder, cooldown time.Duration) *BreakerClient {
	return newBreakerClient(inner, recorder, cooldown)
}

func newBreakerClient(inner *Client, recorder resilience.StateRecorder, cooldown time.Duration) *BreakerClient {
	return &BreakerClient{
		breaker: gobreaker.NewCircuitBreaker[int](gobreaker.Settings{
			Name:        DependencyName,
			MaxRequests: resilience.DefaultMaxRequests,
			Interval:    resilience.DefaultInterval,
			Timeout:     cooldown,
			ReadyToTrip: resilience.ReadyToTrip,
			// The caller giving up (request cancelled) is not this
			// dependency's fault; don't let it count as a failure
			// against the breaker either way.
			IsExcluded:    func(err error) bool { return errors.Is(err, context.Canceled) },
			OnStateChange: resilience.RecordStateChange(DependencyName, recorder),
		}),
		inner:    inner,
		fallback: NewPermissiveClient(),
	}
}

// InstalledCapacity derives its timeout from the inbound request's
// remaining deadline (capped at DefaultTimeout -- see
// resilience.CallTimeout), then routes the call through the breaker.
// While the breaker is OPEN (or half-open and already saturated with
// probes), it falls back to PermissiveClient.InstalledCapacity -- the
// EXISTING fail-loud behaviour, unchanged -- rather than fabricating a
// capacity number.
func (c *BreakerClient) InstalledCapacity(ctx context.Context, capability shared.Capability) (int, error) {
	callCtx, cancel := resilience.CallTimeout(ctx, DefaultTimeout)
	defer cancel()

	v, err := c.breaker.Execute(func() (int, error) {
		return c.inner.InstalledCapacity(callCtx, capability)
	})
	if isBreakerRejection(err) {
		return c.fallback.InstalledCapacity(ctx, capability)
	}
	if err != nil {
		return 0, err
	}
	return v, nil
}

// isBreakerRejection reports whether err is gobreaker refusing to even
// attempt the call (open, or half-open and already at its probe limit)
// -- the ONLY case that means "fall back to the permissive behaviour";
// a real error FROM a call gobreaker did let through must propagate
// unchanged, exactly as it did before this breaker existed.
func isBreakerRejection(err error) bool {
	return errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests)
}
