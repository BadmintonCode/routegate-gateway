SET NAMES utf8mb4;

-- AI assistant (ADR-0001 §10): panel chat over the gateway's read-only data.
-- llm_endpoints stores OpenAI-compatible chat endpoints the assistant may
-- use. api_key is a credential: stored plaintext in MySQL (same convention as
-- alert_channels.webhook_token), masked in every API response and audit row,
-- never logged.
CREATE TABLE IF NOT EXISTS llm_endpoints (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name       VARCHAR(64)     NOT NULL,
  enabled    TINYINT(1)      NOT NULL DEFAULT 1,
  is_default TINYINT(1)      NOT NULL DEFAULT 0,
  base_url   VARCHAR(256)    NOT NULL,
  api_key    VARCHAR(512)    NOT NULL DEFAULT '',
  model      VARCHAR(128)    NOT NULL,
  remark     VARCHAR(256)    NOT NULL DEFAULT '',
  created_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Conversation sessions, one per chat thread. endpoint_id = 0 means "use the
-- current default endpoint"; sessions are owned by the admin user that
-- created them (user_id scoping is enforced in code, no FKs by design).
CREATE TABLE IF NOT EXISTS assistant_sessions (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  session_key VARCHAR(64)     NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL DEFAULT 0,
  user_name   VARCHAR(128)    NOT NULL DEFAULT '',
  title       VARCHAR(256)    NOT NULL DEFAULT '',
  endpoint_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_session_key (session_key),
  INDEX idx_user_updated (user_id, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Chat messages. role: user | assistant. assistant rows carry a tool_calls
-- JSON audit trail (tool name, argument summary, ok, duration) so every
-- read-only tool invocation the model triggered stays attributable.
CREATE TABLE IF NOT EXISTS assistant_messages (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  session_id BIGINT UNSIGNED NOT NULL,
  role       VARCHAR(16)     NOT NULL,
  content    MEDIUMTEXT      NOT NULL,
  tool_calls JSON            NULL,
  created_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX idx_session (session_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
