-- RouteGate initial schema (ADR-0001 §4).
-- utf8mb4 / InnoDB. No foreign keys by design: referential rules are enforced at
-- load time (dangling rows are dropped and counted) and by the Admin API.

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS settings (
  name       VARCHAR(64)  NOT NULL PRIMARY KEY,
  value      JSON         NOT NULL,
  updated_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO settings (name, value) VALUES ('config_revision', CAST(1 AS JSON))
  ON DUPLICATE KEY UPDATE name = name;

CREATE TABLE IF NOT EXISTS routes (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name        VARCHAR(128)    NOT NULL,
  path_prefix VARCHAR(256)    NOT NULL,        -- explicit prefix whitelist; longest-prefix-first match
  methods     VARCHAR(128)    NULL,            -- NULL = all methods; else 'GET,POST'
  targets     JSON            NOT NULL,        -- [{"host":"svc-a","port":8080,"weight":1}]
  strip_prefix TINYINT(1)     NOT NULL DEFAULT 0,
  enabled     TINYINT(1)      NOT NULL DEFAULT 1,
  remark      VARCHAR(255)    NOT NULL DEFAULT '',
  created_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_name (name),
  UNIQUE KEY uk_path_prefix (path_prefix)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS policies (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  route_id     BIGINT UNSIGNED NOT NULL,       -- owning route; delete route first requires deleting its policies
  caller       VARCHAR(128)    NOT NULL,       -- = identities.name; '*' only for temporary rules (expires_at required)
  method       VARCHAR(16)     NOT NULL DEFAULT '*',
  path_pattern VARCHAR(256)    NOT NULL DEFAULT '/**', -- segments after prefix strip; '**' only as last segment
  effect       ENUM('allow','deny') NOT NULL,
  expires_at   DATETIME        NULL,           -- required for caller='*' or pattern='/**'
  enabled      TINYINT(1)      NOT NULL DEFAULT 1,
  created_by   BIGINT UNSIGNED NOT NULL DEFAULT 0,
  remark       VARCHAR(255)    NOT NULL DEFAULT '',
  created_at   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX idx_route_caller (route_id, caller, enabled)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS identities (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name             VARCHAR(128)    NOT NULL,     -- caller principal referenced by policies
  key_id           VARCHAR(64)     NOT NULL,     -- mtls_cert: SHA-256 fingerprint (hex); ed25519: custom kid
  credential_type  ENUM('mtls_cert','ed25519') NOT NULL DEFAULT 'mtls_cert',
  public_material  TEXT            NOT NULL,     -- PEM: leaf certificate (self-signed or externally issued) / Ed25519 public key
  source           ENUM('upload','generated') NOT NULL DEFAULT 'upload',
  status           ENUM('active','revoked') NOT NULL DEFAULT 'active',
  not_before       DATETIME        NULL,
  not_after        DATETIME        NULL,
  revoked_at       DATETIME        NULL,
  revoke_reason    VARCHAR(255)    NOT NULL DEFAULT '',
  created_by       BIGINT UNSIGNED NOT NULL DEFAULT 0,
  remark           VARCHAR(255)    NOT NULL DEFAULT '',
  created_at       DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at       DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_name_kid (name, key_id),
  INDEX idx_name_status (name, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS users (
  id                   BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  username             VARCHAR(128)    NOT NULL,
  password_hash        VARCHAR(255)    NOT NULL,   -- Argon2id PHC string
  display_name         VARCHAR(128)    NOT NULL DEFAULT '',
  role                 ENUM('superadmin','user') NOT NULL DEFAULT 'user',
  status               ENUM('active','disabled') NOT NULL DEFAULT 'active',
  failed_login_count   INT UNSIGNED    NOT NULL DEFAULT 0,
  locked_until         DATETIME        NULL,
  must_change_password TINYINT(1)      NOT NULL DEFAULT 0,
  last_login_at        DATETIME        NULL,
  last_login_ip        VARCHAR(64)     NOT NULL DEFAULT '',
  created_at           DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at           DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS sessions (
  id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  user_id             BIGINT UNSIGNED NOT NULL,
  token_hash          CHAR(64)        NOT NULL,     -- SHA-256 hex of the session token
  created_at          DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at          DATETIME        NOT NULL,
  absolute_expires_at DATETIME        NOT NULL,
  ip                  VARCHAR(64)     NOT NULL DEFAULT '',
  user_agent          VARCHAR(255)    NOT NULL DEFAULT '',
  revoked_at          DATETIME        NULL,
  UNIQUE KEY uk_token_hash (token_hash),
  INDEX idx_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS admin_audit (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  actor_id      BIGINT UNSIGNED NOT NULL DEFAULT 0,
  actor_name    VARCHAR(128)    NOT NULL DEFAULT '',
  action        VARCHAR(64)     NOT NULL,
  resource_type VARCHAR(64)     NOT NULL DEFAULT '',
  resource_id   VARCHAR(64)     NOT NULL DEFAULT '',
  before_state  JSON            NULL,
  after_state   JSON            NULL,
  request_id    VARCHAR(128)    NOT NULL DEFAULT '',
  ip            VARCHAR(64)     NOT NULL DEFAULT '',
  created_at    DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS instances (
  instance_id     VARCHAR(128) NOT NULL PRIMARY KEY,
  version         VARCHAR(64)  NOT NULL DEFAULT '',
  loaded_revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
  started_at      DATETIME     NOT NULL,
  last_sync_at    DATETIME     NULL,
  last_sync_ok    TINYINT(1)   NOT NULL DEFAULT 0,
  updated_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS access_log (
  id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  ts                DATETIME     NOT NULL,
  instance_id       VARCHAR(128) NOT NULL DEFAULT '',
  request_id        VARCHAR(128) NOT NULL DEFAULT '',
  caller            VARCHAR(128) NOT NULL DEFAULT '',
  route_name        VARCHAR(128) NOT NULL DEFAULT '',
  method            VARCHAR(16)  NOT NULL DEFAULT '',
  path              VARCHAR(512) NOT NULL DEFAULT '',
  matched_policy_id BIGINT UNSIGNED NULL,
  config_revision   BIGINT UNSIGNED NOT NULL DEFAULT 0,
  decision          VARCHAR(16)  NOT NULL DEFAULT '',
  upstream_host     VARCHAR(256) NOT NULL DEFAULT '',
  upstream_status   INT          NULL,
  duration_ms       INT UNSIGNED NOT NULL DEFAULT 0,
  INDEX idx_ts (ts),
  INDEX idx_caller_ts (caller, ts)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS schema_migrations (
  version    VARCHAR(32) NOT NULL PRIMARY KEY,
  applied_at DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
