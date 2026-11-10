SET NAMES utf8mb4;

-- Alert firings persist the business message text next to biz_code (task
-- 20261109-routegate-alert-biz-fields, ADR-0001 §9.2): the notification card
-- already carried biz_message, but alert_events dropped it, so the firing
-- history kept the business code without its text. 512 matches the data
-- plane's biz_message extraction cap.
ALTER TABLE alert_events
  ADD COLUMN biz_message VARCHAR(512) NOT NULL DEFAULT '' AFTER biz_code;
