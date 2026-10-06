-- Site-scoped staffing gap (ADR 0034): the optional canonical Site code of the
-- associate's shift (the facility-layout Site code, e.g. WH1). Nullable and
-- additive: every existing row keeps NULL ("site unknown") and counts only in
-- UNSCOPED staffing-gap queries. No backfill, no constraint -- the code is
-- accepted as given, never validated against facility-layout.
ALTER TABLE associate_shift ADD COLUMN site_code TEXT;

-- Partial index: the scoped gap query joins labor_assignment to the
-- associates of one site; legacy NULL rows are never looked up by site.
CREATE INDEX idx_associate_shift_site_code ON associate_shift (site_code) WHERE site_code IS NOT NULL;
