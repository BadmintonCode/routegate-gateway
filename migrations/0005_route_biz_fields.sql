-- Per-route business-envelope fields (owner request 2026-11-07, ADR-0001
-- §6.7): optional dotted JSON paths into the response body telling the data
-- plane where the business status code and message live. Extracted values
-- are written to access records (biz_code / biz_message) so abnormal
-- business responses are visible in the audit. NULL = not configured.
SET NAMES utf8mb4;

ALTER TABLE routes
  ADD COLUMN biz_code_path    VARCHAR(128) NULL,
  ADD COLUMN biz_message_path VARCHAR(128) NULL;
