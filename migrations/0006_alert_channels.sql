-- Alert channels become a managed entity (owner request 2026-11-08): a
-- dedicated table supporting multiple channels, replacing the interim
-- settings.alert_channel row from the previous round (which is migrated
-- here and then removed).
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS alert_channels (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name          VARCHAR(64)     NOT NULL,
  enabled       TINYINT(1)      NOT NULL DEFAULT 0,
  platform      VARCHAR(16)     NOT NULL,
  webhook_token VARCHAR(256)    NOT NULL,
  created_at    DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at    DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Carry over the single channel configured through the interim settings row.
INSERT IGNORE INTO alert_channels (name, enabled, platform, webhook_token)
SELECT 'default',
       IF(JSON_UNQUOTE(JSON_EXTRACT(s.value, '$.enabled')) = 'true', 1, 0),
       COALESCE(NULLIF(JSON_UNQUOTE(JSON_EXTRACT(s.value, '$.platform')), ''), 'feishu'),
       COALESCE(JSON_UNQUOTE(JSON_EXTRACT(s.value, '$.webhook_token')), '')
FROM settings s
WHERE s.name = 'alert_channel'
  AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT(s.value, '$.webhook_token')), '') <> '';

DELETE FROM settings WHERE name = 'alert_channel';
