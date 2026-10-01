// Package laborperformancecache is the outbound adapter that keeps a
// local, in-memory measured-rate read model up to date by consuming
// labor-performance's warehouse.labor-performance.events integration
// topic (ADR 0013 on labor-performance's side), replacing the
// synchronous HTTP call this service used to make for every
// ProposePathPlan enrichment (internal/adapters/outbound/laborperformance,
// ADR-0012). It satisfies ports.MeasuredRateClient (the same interface
// the HTTP client and the permissive no-op already satisfy) and a
// Ready()/WaitReady() gate so this service's composition root can block
// starting real work until the initial replay of the topic's full
// history completes.
//
// This package mirrors internal/adapters/outbound/kafkacatalog
// byte-for-byte in its concurrency/readiness design -- see that
// package's doc comment for the full rationale, including two real bugs
// found and fixed there before this copy was made:
//
//  1. A readiness gate that only re-evaluates "am I caught up" on a NEW
//     message deadlocks forever on an ordinary restart where nothing new
//     has been published since the last run.
//  2. A FIXED, shared consumer group name lets a new process resume from
//     a PRIOR process's committed offset, reporting itself ready with an
//     empty local cache having never actually replayed anything. Fixed
//     here too by making every NewConsumer call use a unique,
//     process-scoped group id -- never a shared name.
//
// # Running-mean strategy
//
// labor-performance's TaskPerformanceRecorded event carries ONE task's
// actual_seconds, not a pre-computed mean -- unlike the HTTP client this
// package replaces, which read labor-performance's own already-computed
// running mean straight off its GetTaskTypePerformance-equivalent
// endpoint. This cache must therefore compute the mean itself. It keeps
// an incremental sum+count per TaskType (Welford-equivalent for a plain
// mean: mean = sum/count, updated in O(1) per message with no history
// retained) rather than storing "most recent N tasks per TaskType" or
// even a single most-recent value. Sum+count was chosen over a bounded
// recent-N window because: (a) TaskType cardinality is small and fixed
// (PICK/PACK/SLAM today), so the memory cost of the running totals is a
// handful of float64/int64 pairs regardless of how many tasks are ever
// recorded -- there is no unbounded-growth risk to trade away by
// windowing; (b) it needs no eviction/ring-buffer logic, so the
// implementation and its failure modes stay as simple as the
// process-path catalogue's map-of-latest-value cache; (c) a single
// most-recent value (the other simple option) would make MeanActualSeconds
// answer "how long did the last task take", a materially different (and
// noisier) question than "what is the measured mean duration" that
// ProposePathPlan actually wants and that the HTTP client's replaced
// endpoint already answered.
//
// A null efficiency_pct does NOT cause a message to be skipped: the
// payload's actual_seconds field (what this cache's mean is built from)
// is always present and non-null per the wire contract; efficiency_pct
// is a wholly separate, unrelated field this cache does not use at all.
// Every TaskPerformanceRecorded message updates its TaskType's running
// mean.
//
// # Idle-share strategy
//
// labor-performance's TaskPerformanceRecorded event additionally carries
// IdleSecondsBefore (wire key idle_seconds_before), a *int64 that is nil
// when the associate's between-task idle gap was not observed: the
// task's first-ever completion for that associate, a negative/zero gap
// from Kafka reordering, or an empty AssociateId (a robot station). This
// cache tracks a running idle share per TaskType using the EXACT SAME
// sum+count/running-totals style as MeanActualSeconds above, for the
// same reasons (small fixed TaskType cardinality, no eviction
// complexity, "what is the measured idle share" not "was the last task
// idle"): it keeps two incremental sums per TaskType, sum(idle) and
// sum(actual), and reports idleShare = sum(idle) / (sum(idle) +
// sum(actual)) -- the fraction of clocked time (task-doing time plus
// idle waiting time) that was idle. A message with a nil
// idle_seconds_before contributes to sum(actual) only (via
// applyTaskPerformanceRecorded's existing mean update) -- it is
// correctly absent from the idle numerator, since "not observed" must
// never be coerced into "zero idle". A TaskType with actual_seconds
// data but no observed idle data yet reports ErrIdleShareUnavailable
// from IdleSharePct, exactly mirroring MeanActualSeconds's own
// no-data-yet contract for MeanActualSeconds/ErrMeasuredRateUnavailable.
package laborperformancecache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// Topic is labor-performance's integration topic — this service has no
// business knowing anything else about that service beyond this topic
// name, the CloudEvents `type` string and the payload shape below (ADR 0013
// on labor-performance's side).
const Topic = "warehouse.labor-performance.events"

// consumerGroupPrefix names this service's dedicated, PER-PROCESS
// consumer group on Topic. Deliberately NOT a fixed, shared name — see
// the package doc comment's bug (2) for why per-process uniqueness is a
// correctness requirement here, not a cosmetic choice.
const consumerGroupPrefix = "workforce-management-labor-performance-cache"

// taskPerformanceData is the CloudEvents `data` payload of
// com.warehouse.wes.labor-performance.performance.TaskPerformanceRecorded
// on Topic (ADR 0013's documented wire contract). EfficiencyPct is
// nullable and unused by this cache; ActualSeconds is always present
// and is the only field the running mean is built from. IdleSecondsBefore
// is nullable ("not observed" -- see the package doc comment's Idle-share
// strategy section); it feeds the running idle-share totals only when
// non-nil.
type taskPerformanceData struct {
	TaskId            string   `json:"task_id"`
	AssociateId       string   `json:"associate_id"`
	TaskType          string   `json:"task_type"`
	EfficiencyPct     *float64 `json:"efficiency_pct"`
	ActualSeconds     int64    `json:"actual_seconds"`
	IdleSecondsBefore *int64   `json:"idle_seconds_before"`
	CompletedAt       string   `json:"completed_at"`
}

// Reader is the subset of *kafkago.Reader this Consumer needs, so tests
// can substitute a fake without a live broker.
type Reader interface {
	ReadMessage(ctx context.Context) (kafkago.Message, error)
	Close() error
}

// Consumer maintains a local, per-TaskType running mean of
// actual_seconds and a running idle share by replaying Topic from its
// earliest offset (a fresh, per-process consumer group) and applying
// every TaskPerformanceRecorded event as it arrives. It satisfies
// ports.MeasuredRateClient via MeanActualSeconds and
// ports.IdleShareClient via IdleSharePct, delegating to the current
// in-memory running totals.
type Consumer struct {
	Reader Reader
	Logger *slog.Logger

	mu         sync.RWMutex
	totals     map[string]runningMean
	idleTotals map[string]idleShareTotals
	ready      bool
	readyCh    chan struct{}
	target     targetOffsets
	seen       map[string]struct{}
}

// runningMean tracks an incremental sum+count for one TaskType, so
// Mean() is O(1) regardless of how many tasks have ever been recorded.
type runningMean struct {
	sum   float64
	count int64
}

func (m runningMean) mean() (float64, bool) {
	if m.count == 0 {
		return 0, false
	}
	return m.sum / float64(m.count), true
}

// idleShareTotals tracks the two running sums the idle-share ratio is
// built from: idleSeconds (numerator, fed only by non-nil
// idle_seconds_before) and actualSeconds (denominator half, fed by
// every message regardless of idle observability). observed reports
// whether at least one message has ever contributed a non-nil
// idle_seconds_before for this TaskType -- a TaskType that only ever
// sees actual_seconds (no idle observation yet) must report
// ErrIdleShareUnavailable, not a fabricated 0 share.
type idleShareTotals struct {
	idleSeconds   float64
	actualSeconds float64
	observed      bool
}

// share computes idleSeconds / (idleSeconds + actualSeconds), the
// fraction of this TaskType's clocked time (task-doing plus idle
// waiting) that was idle. ok is false when no idle observation has ever
// been recorded for this TaskType, or the two sums are both zero
// (nothing to divide).
func (t idleShareTotals) share() (float64, bool) {
	if !t.observed {
		return 0, false
	}
	denom := t.idleSeconds + t.actualSeconds
	if denom <= 0 {
		return 0, false
	}
	return t.idleSeconds / denom, true
}

// targetOffsets is the per-partition "caught up" watermark captured once
// at startup, so Ready() reflects "has this consumer seen everything
// that existed in the topic at the moment it started", not "will it
// ever catch up to a topic that keeps growing".
type targetOffsets map[int]int64

// NewConsumer constructs a Consumer reading the production Topic from
// brokers under a fresh, PROCESS-UNIQUE consumer group, starting at the
// earliest offset. See the package doc comment for why the group must
// never be a fixed shared name.
func NewConsumer(ctx context.Context, brokers []string, logger *slog.Logger) (*Consumer, error) {
	return NewConsumerForTopic(ctx, brokers, Topic, logger)
}

// NewConsumerForTopic constructs the same replay/readiness consumer for
// a caller-supplied topic. Production uses NewConsumer; this constructor
// makes it possible to verify the identical logic against an isolated,
// throwaway Kafka topic in integration tests without connecting to
// shared infrastructure.
func NewConsumerForTopic(ctx context.Context, brokers []string, topic string, logger *slog.Logger) (*Consumer, error) {
	if topic == "" {
		return nil, fmt.Errorf("laborperformancecache: topic must not be empty")
	}
	if logger == nil {
		logger = slog.Default()
	}

	target, err := newTargetOffsets(ctx, brokers, topic)
	if err != nil {
		return nil, fmt.Errorf("laborperformancecache: determine readiness target: %w", err)
	}

	reader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:     brokers,
		Topic:       topic,
		GroupID:     uniqueConsumerGroup(),
		StartOffset: kafkago.FirstOffset,
	})

	c := &Consumer{
		Reader:     reader,
		Logger:     logger,
		totals:     make(map[string]runningMean),
		idleTotals: make(map[string]idleShareTotals),
		readyCh:    make(chan struct{}),
		target:     target,
		seen:       make(map[string]struct{}),
	}
	if len(target) == 0 {
		// The topic has no partitions with any messages yet (a brand
		// new topic, or its producer has never published) — there is
		// nothing to catch up to, so this consumer is trivially ready.
		c.markReady()
	}
	return c, nil
}

// uniqueConsumerGroup builds a group id unique to this process instance
// (hostname + PID + a nanosecond timestamp) — see the package doc
// comment for why per-process uniqueness is a correctness requirement,
// not a cosmetic choice.
func uniqueConsumerGroup() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s-%s-%d-%d", consumerGroupPrefix, host, os.Getpid(), time.Now().UnixNano())
}

// newTargetOffsets dials Topic directly (no consumer group) and reads
// each partition's current last offset, returning only the partitions
// that actually have at least one message.
func newTargetOffsets(ctx context.Context, brokers []string, topic string) (targetOffsets, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("laborperformancecache: no brokers configured")
	}
	conn, err := kafkago.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return nil, fmt.Errorf("laborperformancecache: dial %s: %w", brokers[0], err)
	}
	defer func() { _ = conn.Close() }()

	partitions, err := conn.ReadPartitions(topic)
	if err != nil {
		return nil, fmt.Errorf("laborperformancecache: read partitions for %s: %w", topic, err)
	}

	out := make(targetOffsets, len(partitions))
	for _, p := range partitions {
		pconn, err := kafkago.DialLeader(ctx, "tcp", brokers[0], topic, p.ID)
		if err != nil {
			return nil, fmt.Errorf("laborperformancecache: dial leader for partition %d: %w", p.ID, err)
		}
		first, last, err := pconn.ReadOffsets()
		closeErr := pconn.Close()
		if err != nil {
			return nil, fmt.Errorf("laborperformancecache: read offsets for partition %d: %w", p.ID, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("laborperformancecache: close leader conn for partition %d: %w", p.ID, closeErr)
		}
		if last > first {
			// last is exclusive (the offset of the NEXT message to be
			// written) — a consumer has caught up once it has
			// processed the message at offset last-1.
			out[p.ID] = last
		}
	}
	return out, nil
}

// Close releases the underlying Kafka reader.
func (c *Consumer) Close() error {
	return c.Reader.Close()
}

// Ready reports whether this consumer has processed every message that
// existed in Topic at the moment it started. Safe to call concurrently.
func (c *Consumer) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// WaitReady blocks until Ready() would return true or ctx is done,
// whichever comes first. Used by the composition root to gate starting
// real work on cache completeness.
func (c *Consumer) WaitReady(ctx context.Context) error {
	if c.Ready() {
		return nil
	}
	select {
	case <-c.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Consumer) markReady() {
	c.mu.Lock()
	alreadyReady := c.ready
	c.ready = true
	c.mu.Unlock()
	if !alreadyReady {
		close(c.readyCh)
	}
}

// taskTypeForPathId maps this context's lowercase PathId (e.g. "pack",
// "pick") onto labor-performance's uppercase TaskType enum (PICK, PACK,
// SLAM). This is the SAME mapping table
// internal/adapters/outbound/laborperformance's HTTP client uses in its
// own taskTypeForPathId — duplicated here rather than imported because
// that function is unexported in its own package (Go's package
// visibility), but it must and does stay byte-for-byte identical. Not
// every PathId has a TaskType counterpart (e.g. "stow", "hazmat" are
// workforce-management-only paths with no measurable task type in
// labor-performance) — those report ok=false so the caller can fail
// fast without a cache lookup.
func taskTypeForPathId(pathId shared.PathId) (taskType string, ok bool) {
	switch strings.ToUpper(string(pathId)) {
	case "PICK", "PACK", "SLAM":
		return strings.ToUpper(string(pathId)), true
	default:
		return "", false
	}
}

// MeanActualSeconds satisfies ports.MeasuredRateClient against this
// consumer's current in-memory running means. Returns
// ports.ErrMeasuredRateUnavailable (never any other error) on ANY
// failure to produce a real value -- pathId has no TaskType counterpart,
// or no TaskPerformanceRecorded message for that TaskType has been
// observed yet -- so ProposePathPlan's single error-handling branch
// never needs to distinguish those cases; all of them mean the same
// thing to a caller: fall back to a caller-supplied rate. This preserves
// the exact fail-open contract internal/adapters/outbound/laborperformance's
// HTTP client and PermissiveClient already implement.
func (c *Consumer) MeanActualSeconds(_ context.Context, pathId shared.PathId) (float64, error) {
	taskType, ok := taskTypeForPathId(pathId)
	if !ok {
		return 0, fmt.Errorf("%w: path %q has no labor-performance task type", ports.ErrMeasuredRateUnavailable, pathId)
	}

	c.mu.RLock()
	totals, tracked := c.totals[taskType]
	c.mu.RUnlock()
	if !tracked {
		return 0, fmt.Errorf("%w: no measured rate observed yet for task type %q", ports.ErrMeasuredRateUnavailable, taskType)
	}
	mean, ok := totals.mean()
	if !ok {
		return 0, fmt.Errorf("%w: no measured rate observed yet for task type %q", ports.ErrMeasuredRateUnavailable, taskType)
	}
	return mean, nil
}

// IdleSharePct satisfies ports.IdleShareClient against this consumer's
// current in-memory running idle-share totals. Returns
// ports.ErrIdleShareUnavailable (never any other error) on ANY failure
// to produce a real value -- pathId has no TaskType counterpart, or no
// TaskPerformanceRecorded message carrying a non-nil idle_seconds_before
// has been observed yet for that TaskType -- mirroring
// MeanActualSeconds's fail-open contract exactly, so both GetStaffingGap
// and ProposePathPlan can treat "no idle signal" uniformly regardless of
// why.
func (c *Consumer) IdleSharePct(_ context.Context, pathId shared.PathId) (float64, error) {
	taskType, ok := taskTypeForPathId(pathId)
	if !ok {
		return 0, fmt.Errorf("%w: path %q has no labor-performance task type", ports.ErrIdleShareUnavailable, pathId)
	}

	c.mu.RLock()
	totals, tracked := c.idleTotals[taskType]
	c.mu.RUnlock()
	if !tracked {
		return 0, fmt.Errorf("%w: no idle share observed yet for task type %q", ports.ErrIdleShareUnavailable, taskType)
	}
	share, ok := totals.share()
	if !ok {
		return 0, fmt.Errorf("%w: no idle share observed yet for task type %q", ports.ErrIdleShareUnavailable, taskType)
	}
	return share, nil
}

// Run consumes Topic until ctx is cancelled or the reader returns a
// fatal error. A handling error is logged and the loop continues, so one
// malformed message cannot wedge this consumer. A message that is not a
// valid CloudEvents 1.0 event (including the retired flat envelope) is
// logged at WARN and skipped — this replay consumer has no DLQ (ADR-0026).
func (c *Consumer) Run(ctx context.Context) error {
	for {
		msg, err := c.Reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := c.handle(msg); errors.Is(err, cloudevents.ErrNotCloudEvent) {
			c.Logger.WarnContext(ctx, "labor-performance cache: skipping message that is not a valid CloudEvents 1.0 event",
				"topic", msg.Topic, "offset", msg.Offset, "partition", msg.Partition, "error", err)
		} else if err != nil {
			c.Logger.ErrorContext(ctx, "labor-performance cache message handling failed",
				"topic", msg.Topic, "offset", msg.Offset, "partition", msg.Partition, "error", err)
		}
		c.checkReady(msg)
	}
}

func (c *Consumer) checkReady(msg kafkago.Message) {
	c.mu.RLock()
	target, tracked := c.target[msg.Partition]
	alreadyReady := c.ready
	c.mu.RUnlock()
	if alreadyReady || !tracked {
		return
	}
	if msg.Offset+1 < target {
		return
	}

	c.mu.Lock()
	delete(c.target, msg.Partition)
	allCaughtUp := len(c.target) == 0
	c.mu.Unlock()

	if allCaughtUp {
		c.markReady()
	}
}

// handle decodes msg as a CloudEvents 1.0 event, dispatches on its FULL
// `type` string, and dedupes on the CloudEvents `id`: unlike the catalogue's
// latest-value cache, the running mean is a SUM, so an at-least-once
// redelivery of the same occurrence must not be folded in twice.
func (c *Consumer) handle(msg kafkago.Message) error {
	e, err := cloudevents.Decode(msg.Value)
	if err != nil {
		return fmt.Errorf("laborperformancecache: %w", err)
	}

	switch e.Type() {
	case cloudevents.TypeTaskPerformanceRecorded:
		var data taskPerformanceData
		if err := e.DataAs(&data); err != nil {
			return fmt.Errorf("laborperformancecache: decode %s data: %w", e.Type(), err)
		}
		if !c.markSeen(e.Source(), e.ID()) {
			return nil
		}
		c.applyTaskPerformanceRecorded(data)
	default:
		// An event type outside this consumer's contract — ignored,
		// same convention as every other consumer in this fleet that
		// reads a shared/fan-out-shaped topic. ADR 0013 scopes this
		// topic to TaskPerformanceRecorded only today, but a future,
		// additive widening on labor-performance's side must not
		// break this consumer.
	}
	return nil
}

// markSeen records (source, id) and reports whether it was new. The set is
// in-memory only, like the totals it protects: a restart replays the topic
// from FirstOffset into empty totals and an empty set together.
func (c *Consumer) markSeen(source, id string) bool {
	key := source + "\x00" + id
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]struct{})
	}
	if _, dup := c.seen[key]; dup {
		return false
	}
	c.seen[key] = struct{}{}
	return true
}

// applyTaskPerformanceRecorded folds one task's actual_seconds into its
// TaskType's running mean, and -- when idle_seconds_before is non-nil --
// its idle-share totals. EfficiencyPct is intentionally never inspected:
// a null efficiency_pct means "unscorable", not "no duration data" --
// actual_seconds is always present per the wire contract, so every
// message updates the mean regardless of whether the task was scorable.
// A nil IdleSecondsBefore still contributes actual_seconds to the
// idle-share denominator (via the totals.actualSeconds update below) but
// leaves totals.observed and the numerator untouched -- "not observed"
// is never coerced into "zero idle" (see the package doc comment's
// Idle-share strategy section).
func (c *Consumer) applyTaskPerformanceRecorded(data taskPerformanceData) {
	taskType := strings.ToUpper(data.TaskType)
	if taskType == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.totals[taskType]
	t.sum += float64(data.ActualSeconds)
	t.count++
	c.totals[taskType] = t

	idle := c.idleTotals[taskType]
	idle.actualSeconds += float64(data.ActualSeconds)
	if data.IdleSecondsBefore != nil {
		idle.idleSeconds += float64(*data.IdleSecondsBefore)
		idle.observed = true
	}
	c.idleTotals[taskType] = idle
}

// Compile-time assertions that Consumer satisfies both outbound ports
// it backs in kafka-cache mode.
var (
	_ ports.MeasuredRateClient = (*Consumer)(nil)
	_ ports.IdleShareClient    = (*Consumer)(nil)
)

// WaitReadyTimeout bounds how long the composition root waits for the
// initial replay before giving up and failing startup outright.
const WaitReadyTimeout = 60 * time.Second
