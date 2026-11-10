-- Operations become a managed entity (owner request 2026-11-06): dedicated
-- table, migrated from the temporary routes.operations JSON column introduced
-- in 0002, which is then dropped.
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS route_operations (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  route_id   BIGINT UNSIGNED NOT NULL,
  method     VARCHAR(16)     NOT NULL DEFAULT '*',
  path       VARCHAR(256)    NOT NULL,
  name       VARCHAR(128)    NOT NULL DEFAULT '',
  remark     VARCHAR(255)    NOT NULL DEFAULT '',
  created_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY uk_route_method_path (route_id, method, path),
  INDEX idx_route (route_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Carry over anything registered through the interim JSON column.
INSERT IGNORE INTO route_operations (route_id, method, path, name)
SELECT r.id,
       COALESCE(NULLIF(UPPER(JSON_UNQUOTE(JSON_EXTRACT(op.value, '$.method'))), ''), '*'),
       JSON_UNQUOTE(JSON_EXTRACT(op.value, '$.path')),
       COALESCE(JSON_UNQUOTE(JSON_EXTRACT(op.value, '$.name')), '')
FROM routes r
JOIN JSON_TABLE(r.operations, '$[*]' COLUMNS (value JSON PATH '$')) op
WHERE r.operations IS NOT NULL AND JSON_TYPE(r.operations) = 'ARRAY';

ALTER TABLE routes DROP COLUMN operations;
