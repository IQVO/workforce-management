package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
)

// Sink is where OutboxRelay forwards drained rows — in production the
// kafka.RelaySink (a topic-less writer that honours each message's own
// topic); in tests a recorder.
type Sink interface {
	Send(ctx context.Context, msgs ...outboundkafka.Encoded) error
}

// OutboxRelay drains unpublished outbox_events rows onto a Sink, oldest
// first, marking each row published as it goes (ADR 0016). One relay
// serves both the integration and the analytics topic: the topic is a
// column on the row, not a property of the relay.
//
// Delivery is at-least-once: a crash between a successful Send and the
// row's UPDATE republishes that row on the next pass. Both consumers
// tolerate this — the analytics projector is idempotent on event_id
// (ADR 0010) and the integration consumers key their read models by
// path_id, so a duplicate is an idempotent overwrite. Per-key ordering is
// preserved because rows are drained in id order within one relay and
// FOR UPDATE SKIP LOCKED keeps two relays (a rolling deploy's overlapping
// old and new pod) from claiming the same row.
//
// A row that keeps failing is capped by WithMaxAttempts (ADR 0036, gap fix
// D12): once it has failed that many times it is excluded from future
// claims, so one poison row cannot stop the whole relay forever. Such a row
// stays in outbox_events with its attempts and last_error for inspection;
// later rows with the same key overtake it (the documented price of the
// cap).
type OutboxRelay struct {
	pool        *pgxpool.Pool
	sink        Sink
	logger      *slog.Logger
	interval    time.Duration
	batchSize   int
	maxAttempts int
}

// RelayOption customises an OutboxRelay.
type RelayOption func(*OutboxRelay)

// WithInterval sets how long the relay sleeps between passes when the
// last pass found nothing to publish. Default 1s.
func WithInterval(d time.Duration) RelayOption {
	return func(r *OutboxRelay) {
		if d > 0 {
			r.interval = d
		}
	}
}

// WithBatchSize caps how many rows one pass claims. Default 100.
func WithBatchSize(n int) RelayOption {
	return func(r *OutboxRelay) {
		if n > 0 {
			r.batchSize = n
		}
	}
}

// WithMaxAttempts caps how many failed sends a row may accumulate before
// the relay stops claiming it. 0 (the default) or a negative value means
// unlimited retries, the pre-cap behaviour. A row that reaches the cap is
// left unpublished in outbox_events with attempts and last_error, logged
// once at ERROR with its id, topic, event type and key, and re-armed by an
// operator with `UPDATE outbox_events SET attempts = 0 WHERE id = ...`
// (ADR 0036). Every composition root passes it (fitness test
// TestOutboxRelaysPassAttemptCap).
func WithMaxAttempts(n int) RelayOption {
	return func(r *OutboxRelay) { r.maxAttempts = n }
}

// NewOutboxRelay constructs a relay draining pool into sink.
func NewOutboxRelay(pool *pgxpool.Pool, sink Sink, logger *slog.Logger, opts ...RelayOption) *OutboxRelay {
	if logger == nil {
		logger = slog.Default()
	}
	r := &OutboxRelay{pool: pool, sink: sink, logger: logger, interval: time.Second, batchSize: 100}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run drains the outbox until ctx is cancelled. A pass that publishes a
// full batch is followed immediately by another pass (there is probably
// more waiting); an empty pass sleeps for the configured interval. A
// failing pass is logged and retried after the interval — the rows stay
// unpublished, so nothing is lost.
func (r *OutboxRelay) Run(ctx context.Context) error {
	for {
		n, err := r.RelayOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			r.logger.ErrorContext(ctx, "outbox relay pass failed", "error", err)
		}
		if n == r.batchSize && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.interval):
		}
	}
}

// pendingOutboxRow is one claimed, decoded outbox_events row awaiting
// publication in the current pass.
type pendingOutboxRow struct {
	id  int64
	enc outboundkafka.Encoded
}

// RelayOnce performs a single pass: claim up to batchSize unpublished rows
// under a row lock, send each ONE AT A TIME in id order, and mark it
// published. It returns how many rows were published. On the first Send
// failure the pass stops (preserving per-key ordering — a later event for
// the same aggregate must not overtake a failed earlier one, until that
// earlier one reaches the WithMaxAttempts cap and is parked), records the
// error on that row, commits what was already sent, and returns the
// error; rows already sent in this pass stay marked published.
func (r *OutboxRelay) RelayOnce(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres: begin relay pass: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch, err := r.claim(ctx, tx)
	if err != nil {
		return 0, err
	}
	return r.flush(ctx, tx, batch)
}

// claim locks up to batchSize unpublished outbox_events rows under tx (FOR
// UPDATE SKIP LOCKED, so a rolling deploy's overlapping relays never claim
// the same row) and decodes them, oldest first. With maxAttempts > 0 a row
// that already failed maxAttempts times is no longer claimed.
func (r *OutboxRelay) claim(ctx context.Context, tx pgx.Tx) ([]pendingOutboxRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, topic, event_type, key, value, headers
		FROM outbox_events
		WHERE published_at IS NULL
		  AND ($2::int <= 0 OR attempts < $2::int)
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, r.batchSize, r.maxAttempts)
	if err != nil {
		return nil, fmt.Errorf("postgres: claim outbox rows: %w", err)
	}
	var batch []pendingOutboxRow
	for rows.Next() {
		var p pendingOutboxRow
		var headers []byte
		if err := rows.Scan(&p.id, &p.enc.Topic, &p.enc.EventType, &p.enc.Key, &p.enc.Value, &headers); err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres: scan outbox row: %w", err)
		}
		if p.enc.Headers, err = unmarshalHeaders(headers); err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres: decode outbox row %d headers: %w", p.id, err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: read outbox rows: %w", err)
	}
	return batch, nil
}

// flush sends each claimed row to the sink one at a time in id order and
// marks it published, then commits the pass. On the first Send failure it
// stops (via failRow) and returns how many rows had already been published.
func (r *OutboxRelay) flush(ctx context.Context, tx pgx.Tx, batch []pendingOutboxRow) (int, error) {
	published := 0
	for _, p := range batch {
		if err := r.sink.Send(ctx, p.enc); err != nil {
			return published, r.failRow(ctx, tx, p, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE outbox_events SET published_at = now(), attempts = attempts + 1, last_error = NULL WHERE id = $1
		`, p.id); err != nil {
			return published, fmt.Errorf("postgres: mark outbox row %d published: %w", p.id, err)
		}
		published++
	}
	if err := tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return published, fmt.Errorf("postgres: commit relay pass: %w", err)
	}
	if published > 0 {
		r.logger.DebugContext(ctx, "outbox relay published events", "count", published)
	}
	return published, nil
}

// failRow records err on the row that failed to send, commits the pass (so
// rows already sent stay marked published and the failure is persisted),
// and returns the wrapped send error. When this failure brings the row to
// the attempt cap it is reported once at ERROR, after the commit: from the
// next pass on it is no longer claimed.
func (r *OutboxRelay) failRow(ctx context.Context, tx pgx.Tx, p pendingOutboxRow, err error) error {
	attempts := 0
	if uerr := tx.QueryRow(ctx, `
		UPDATE outbox_events SET attempts = attempts + 1, last_error = $2 WHERE id = $1 RETURNING attempts
	`, p.id, err.Error()).Scan(&attempts); uerr != nil {
		err = errors.Join(err, fmt.Errorf("postgres: record outbox failure: %w", uerr))
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		err = errors.Join(err, fmt.Errorf("postgres: commit relay pass: %w", cerr))
	} else if r.maxAttempts > 0 && attempts >= r.maxAttempts {
		r.logger.ErrorContext(ctx, "outbox relay: row reached the attempt cap and is no longer claimed",
			"outbox_id", p.id, "topic", p.enc.Topic, "event_type", p.enc.EventType, "key", string(p.enc.Key),
			"attempts", attempts, "max_attempts", r.maxAttempts, "error", err,
			"rearm", "UPDATE outbox_events SET attempts = 0 WHERE id = <outbox_id>")
	}
	return fmt.Errorf("outbox relay: send %s to %s (row %d): %w", p.enc.EventType, p.enc.Topic, p.id, err)
}
