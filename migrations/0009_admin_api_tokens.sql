-- User-scoped temporary admin API tokens (owner request 2026-11-09): Bearer
-- credentials for AI/machine clients that authenticate AS the issuing user.
-- The token carries no independent permission scope — the user's role and
-- status are resolved live from the users row on every request, so role
-- changes and disabling the user take effect immediately. Only the SHA-256
-- hash of the secret is stored; the raw token is shown once at mint time.
-- Conventions mirror the admin sessions table in 0001_init.sql (plain
-- DATETIME precision, utf8mb4, InnoDB, no foreign keys).
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS admin_api_tokens (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  user_id      BIGINT UNSIGNED NOT NULL,       -- the identity the token acts as
  name         VARCHAR(64)     NOT NULL,       -- display name chosen at mint time
  token_hash   CHAR(64)        NOT NULL,       -- SHA-256 hex of the raw token secret
  prefix       VARCHAR(16)     NOT NULL,       -- first 12 chars of the raw token (rgt_…, display only)
  created_at   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at   DATETIME        NOT NULL,       -- mandatory: tokens are always temporary
  last_used_at DATETIME        NULL,
  revoked_at   DATETIME        NULL,
  UNIQUE KEY uk_token_hash (token_hash),
  INDEX idx_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
