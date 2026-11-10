-- NOTE (2026-11-08): this migration is superseded by 0003_route_operations_table,
-- which migrates the data added here into a proper table and drops the column
-- created by this file. It is kept only for databases that already ran it.
-- Fresh installs execute 0002 and 0003 in order — that is expected.
--
-- Route-level operation registry (owner request 2026-11-06): each route may
-- register the concrete operations it supports so the policy editor can offer
-- them as selectable path patterns. Enforcement still comes from policies.
ALTER TABLE routes ADD COLUMN operations JSON NULL AFTER targets;
