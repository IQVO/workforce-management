-- Idempotency-Key middleware (see docs/docs/adr/0027-idempotency-key-middleware.md).
--
-- One row per Idempotency-Key value ever seen on a protected route. The
-- middleware INSERTs a bare row (status_code/response_body/response_headers
-- all NULL) inside its own transaction before calling the real handler,
-- then UPDATEs that same row with the outcome and commits — all in ONE
-- transaction shared with the use case's own aggregate write and outbox
-- insert (see unit_of_work.go's pgtx binding). A reader can therefore never
-- observe a COMMITTED row with a NULL status_code: either the row was never
-- committed at all (the inserting transaction rolled back), or it was
-- committed only after the UPDATE populated every outcome column.
CREATE TABLE idempotency_keys (
    key              TEXT PRIMARY KEY,
    method           TEXT NOT NULL,
    path             TEXT NOT NULL,
    request_hash     TEXT NOT NULL,
    status_code      INTEGER,
    response_body    BYTEA,
    response_headers JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ
);

-- The housekeeping sweeper (ADR-0028) deletes rows older than the TTL
-- (IDEMPOTENCY_KEY_TTL, default 24h); this index keeps that scan cheap.
CREATE INDEX idx_idempotency_keys_created_at ON idempotency_keys (created_at);

-- The sweeper also deletes PUBLISHED outbox rows past their retention
-- (OUTBOX_RETENTION, default 7d). The existing partial index only covers the
-- unpublished tail, so give the published scan its own.
CREATE INDEX idx_outbox_events_published_at ON outbox_events (published_at) WHERE published_at IS NOT NULL;
