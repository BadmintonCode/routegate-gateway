-- Per-route success verdict rule (owner request 2026-11-07, ADR-0001 §6.7):
-- a composable JSON rule over the HTTP status and response-body fields that
-- decides whether a proxied request counts as success in the audit. NULL =
-- no rule (the audit shows no verdict for that route). Route-level by
-- decision; the JSON shape reserves room for a future per-operation scope.
SET NAMES utf8mb4;

ALTER TABLE routes ADD COLUMN success_rule JSON NULL;
