SET NAMES utf8mb4;

-- AI Agent schema reset: no production AI Agent data exists yet, so the
-- one-to-one endpoint/model layout from migration 0010 is intentionally
-- replaced by a clean endpoint -> model-binding hierarchy.
DROP TABLE IF EXISTS assistant_messages;
DROP TABLE IF EXISTS assistant_sessions;
DROP TABLE IF EXISTS llm_endpoint_models;
DROP TABLE IF EXISTS llm_endpoints;

-- Connection configuration only. A single endpoint can expose many models.
CREATE TABLE llm_endpoints (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name       VARCHAR(64)     NOT NULL,
  enabled    TINYINT(1)      NOT NULL DEFAULT 1,
  base_url   VARCHAR(256)    NOT NULL,
  api_key    VARCHAR(512)    NOT NULL DEFAULT '',
  remark     VARCHAR(256)    NOT NULL DEFAULT '',
  created_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_llm_endpoint_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- A concrete model exposed by one endpoint. is_default is global: the
-- write paths enforce at most one enabled default model.
CREATE TABLE llm_endpoint_models (
  id                   BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  endpoint_id          BIGINT UNSIGNED NOT NULL,
  provider_model_name  VARCHAR(128)    NOT NULL,
  display_name         VARCHAR(128)    NOT NULL,
  enabled              TINYINT(1)     NOT NULL DEFAULT 1,
  is_default            TINYINT(1)     NOT NULL DEFAULT 0,
  max_tokens            INT UNSIGNED   NULL,
  temperature           DECIMAL(5,4)   NULL,
  remark                VARCHAR(256)   NOT NULL DEFAULT '',
  created_at           DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at           DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_endpoint_provider_model (endpoint_id, provider_model_name),
  INDEX idx_endpoint_enabled (endpoint_id, enabled),
  INDEX idx_default_enabled (is_default, enabled)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE assistant_sessions (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  session_key VARCHAR(64)     NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL DEFAULT 0,
  user_name   VARCHAR(128)    NOT NULL DEFAULT '',
  title       VARCHAR(256)    NOT NULL DEFAULT '',
  model_id    BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_assistant_session_key (session_key),
  INDEX idx_assistant_user_updated (user_id, updated_at),
  INDEX idx_assistant_model (model_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE assistant_messages (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  session_id BIGINT UNSIGNED NOT NULL,
  role       VARCHAR(16)     NOT NULL,
  content    MEDIUMTEXT      NOT NULL,
  tool_calls JSON            NULL,
  created_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX idx_assistant_message_session (session_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
