-- Local copy of warehouse-planning's LabourPlanCommitted (ADR 0043 decision 1):
-- one row per (site_code, shift_id, path_id), the heads the demand requires.
-- site_code is the wire site_id, a facility-layout site code (ADR 0035).
-- version is warehouse-planning's per-key version: a row is replaced only by a
-- strictly greater one (the upsert's WHERE guard), so redelivered or reordered
-- older messages never roll the copy back.
CREATE TABLE labour_requirements (
    site_code           TEXT NOT NULL,
    shift_id            TEXT NOT NULL,
    path_id             TEXT NOT NULL,
    planned_heads       INTEGER NOT NULL CHECK (planned_heads >= 0),
    rate_units_per_hour DOUBLE PRECISION NOT NULL,
    hours               DOUBLE PRECISION NOT NULL,
    heads_source        TEXT NOT NULL,
    version             INTEGER NOT NULL CHECK (version >= 1),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (site_code, shift_id, path_id)
);

-- The labour-plan consumer's claim on the CloudEvents id: inserted in the SAME
-- transaction as the labour_requirements upsert, so a rolled-back apply also
-- releases the claim and the redelivered message is applied again. Same shape
-- as the analytics consumer's analytics_consumed_events.
CREATE TABLE labour_plan_consumed_events (
    event_id     TEXT PRIMARY KEY,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
