-- Alert rules (owner request 2026-11-08, ADR-0001 §9.2): composable response
-- conditions (same shape as route success rules, plus an outcome source fed
-- by the route's own success verdict) scoped to all or selected routes,
-- firing into exactly one alert channel. Every firing is recorded in
-- alert_events (no noise suppression / rate limiting in v1 — explicit owner
-- decision).
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS alert_rules (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name        VARCHAR(64)     NOT NULL,
  enabled     TINYINT(1)      NOT NULL DEFAULT 1,
  scope_type  ENUM('all','routes') NOT NULL DEFAULT 'all',
  route_ids   JSON NULL,
  channel_id  BIGINT UNSIGNED NOT NULL,
  match_mode  ENUM('all','any') NOT NULL DEFAULT 'all',
  conditions  JSON NOT NULL,
  created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_name (name),
  INDEX idx_channel (channel_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS alert_events (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  rule_id     BIGINT UNSIGNED NOT NULL,
  rule_name   VARCHAR(64)     NOT NULL DEFAULT '',
  channel_id  BIGINT UNSIGNED NOT NULL DEFAULT 0,
  route_name  VARCHAR(64)     NOT NULL DEFAULT '',
  request_id  VARCHAR(128)    NOT NULL DEFAULT '',
  http_status INT             NOT NULL DEFAULT 0,
  outcome     VARCHAR(16)     NOT NULL DEFAULT '',
  biz_code    VARCHAR(128)    NOT NULL DEFAULT '',
  detail      JSON NULL,
  sent        TINYINT(1)      NOT NULL DEFAULT 0,
  send_error  VARCHAR(255)    NOT NULL DEFAULT '',
  created_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX idx_rule (rule_id, id),
  INDEX idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
