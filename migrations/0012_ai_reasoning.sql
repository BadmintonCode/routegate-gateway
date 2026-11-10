SET NAMES utf8mb4;

-- Persist the model's reasoning chain (thinking deltas collected across all
-- agent steps) alongside each assistant answer so the panel can review the
-- thinking process after the turn completes.
--
-- Idempotent via a guarded prepared statement: schema_migrations.version is
-- VARCHAR(32), so migration FILENAMES (including .sql) must stay <= 32 chars
-- — an earlier draft of this migration used a longer name, and on hosts where
-- its ALTER already applied but the version row failed to record, a plain
-- ADD COLUMN would crash-loop on restart with "Duplicate column".
SET @col_exists = (
  SELECT COUNT(*) FROM information_schema.COLUMNS
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME = 'assistant_messages'
    AND COLUMN_NAME = 'reasoning'
);
SET @ddl = IF(@col_exists = 0,
  'ALTER TABLE assistant_messages ADD COLUMN reasoning MEDIUMTEXT NULL AFTER content',
  'SELECT 1');
PREPARE ai_reasoning_stmt FROM @ddl;
EXECUTE ai_reasoning_stmt;
DEALLOCATE PREPARE ai_reasoning_stmt;
