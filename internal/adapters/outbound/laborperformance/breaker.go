// breaker.go wraps Client with a per-dependency circuit breaker
// (sony/gobreaker/v2, ADR-0022) AND jittered retry (cenkalti/backoff/v4)
// -- the ONLY outbound client in this service that retries, because
// MeanActualSeconds is a pure GET/read, safe to retry unlike
// fulfillment-execution's mutating-adjacent capacity check gating a
// commit (see fulfillmentexecution/breaker.go's doc comment for that
// decision, and the ADR for the full reasoning). While the breaker is
// OPEN, this falls back to PermissiveClient's existing fail-OPEN
// behaviour (ErrMeasuredRateUnavailable) -- the SAME fallback this
// client already had for a transport error, a malformed response, or
// genuinely no data yet, just now also reachable via the breaker
// short-circuiting a call it never attempts.
package laborperformance

import (
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v4"
	gobreaker "github.com/sony/gobreaker/v2"

	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/resilience"
)

// DependencyName labels this breaker's Prometheus gauge series
// (circuit_breaker_state{dependency="labor-performance"}).
const DependencyName = "labor-performance"

// maxRetryAttempts caps the jittered retry at 3 total attempts (1
// original + 2 retries) per the plan's "max 3 attempts" bound.
const maxRetryAttempts = 3

// retryInitialInterval/retryMaxInterval bound the exponential-backoff-
// with-jitter schedule between attempts -- short, because this whole
// call is already bounded by DefaultTimeout end to end (see
// GetClassification's resilience.CallTimeout use in order-management,
// mirrored here).
const (
	retryInitialInterval = 50 * time.Millisecond
	retryMaxInterval     = 500 * time.Millisecond
)

// BreakerClient wraps Client with retry-then-circuit-breaker for
// MeanActualSeconds. This dependency has exactly one outbound call, so
// this type stays single-purpose (mirroring order-management's
// productclassification.BreakerClient shape).
type BreakerClient struct {
	breaker  *gobreaker.CircuitBreaker[float64]
	inner    *Client
	fallback *PermissiveClient
}

var _ ports.MeasuredRateClient = (*BreakerClient)(nil)

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
		breaker: gobreaker.NewCircuitBreaker[float64](gobreaker.Settings{
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

// MeanActualSeconds derives its timeout from the inbound request's
// remaining deadline (capped at DefaultTimeout), retries fetch up to
// maxRetryAttempts times with jittered backoff, and runs the whole
// retry loop through the breaker as ONE logical call -- a retry storm
// against an already-degraded dependency still only ever counts as one
// success/failure toward the breaker's trip condition, not N. While the
// breaker is OPEN (or half-open and saturated), this falls back to
// PermissiveClient -- the SAME fail-open contract MeanActualSeconds
// already had, just reached via a different path.
func (c *BreakerClient) MeanActualSeconds(ctx context.Context, pathId shared.PathId) (float64, error) {
	callCtx, cancel := resilience.CallTimeout(ctx, DefaultTimeout)
	defer cancel()

	result, err := c.breaker.Execute(func() (float64, error) {
		return c.retryingFetch(callCtx, pathId)
	})
	if isBreakerRejection(err) {
		return c.fallback.MeanActualSeconds(ctx, pathId)
	}
	if err != nil {
		// inner.MeanActualSeconds never returns anything other than
		// ports.ErrMeasuredRateUnavailable (see client.go's doc
		// comment) -- this branch mirrors that same fail-open
		// contract for a retry-exhausted call, so
		// ProposePathPlan's single error-handling branch never
		// needs to distinguish "breaker open" from "genuinely no
		// data yet".
		return 0, err
	}
	return result, nil
}

// retryingFetch retries inner.MeanActualSeconds with jittered
// exponential backoff, bounded to maxRetryAttempts total attempts and
// to callCtx's own deadline (whichever is tighter). A pathId with no
// TaskType counterpart returns ports.ErrMeasuredRateUnavailable
// immediately without ever calling the HTTP doer (see
// taskTypeForPathId) -- backoff.Permanent marks that case
// non-retryable so it never consumes retry budget the same way a real
// transport failure would.
func (c *BreakerClient) retryingFetch(callCtx context.Context, pathId shared.PathId) (float64, error) {
	if _, ok := taskTypeForPathId(pathId); !ok {
		return 0, ports.ErrMeasuredRateUnavailable
	}

	policy := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(retryInitialInterval),
		backoff.WithMaxInterval(retryMaxInterval),
	)
	bounded := backoff.WithContext(backoff.WithMaxRetries(policy, maxRetryAttempts-1), callCtx)

	return backoff.RetryNotifyWithData(func() (float64, error) {
		return c.inner.MeanActualSeconds(callCtx, pathId)
	}, bounded, nil)
}

// isBreakerRejection reports whether err is gobreaker refusing to even
// attempt the call (open, or half-open and already at its probe limit)
// -- the ONLY case that means "fall back to the permissive behaviour";
// a real error FROM a call gobreaker did let through must propagate
// unchanged, exactly as it did before this breaker existed.
func isBreakerRejection(err error) bool {
	return errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests)
}
