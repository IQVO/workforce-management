-- Optimistic concurrency: a version column on the two mutable aggregates
-- whose repo Save is a genuine read-modify-write (see ADR 0021). ShiftPlan
-- is deliberately excluded -- CommitShiftPlan always constructs a brand
-- new, self-contained ShiftPlan from caller-supplied lines rather than
-- loading and mutating the previous one, so there is no stale-field
-- clobber hazard to guard against there.
ALTER TABLE associate_shift ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE labor_assignment ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
