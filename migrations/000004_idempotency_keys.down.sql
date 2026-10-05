DROP INDEX IF EXISTS idx_outbox_events_published_at;
DROP INDEX IF EXISTS idx_idempotency_keys_created_at;
DROP TABLE IF EXISTS idempotency_keys;
