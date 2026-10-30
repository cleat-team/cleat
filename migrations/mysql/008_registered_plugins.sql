-- cleat#3600 (owner D52): which plugin NAMES some worker has registered, at least once. See
-- migrations/postgres/008 for the full reasoning; this is the same change.
--
-- MySQL has no schemas, so the table is unprefixed, like workers. CREATE TABLE IF NOT EXISTS is
-- already safe to run twice, so no guard procedure is needed. There is no 005, 006 or 007 on
-- MySQL: the numbers are shared across dialects and MySQL has no counterpart for those changes
-- (007 belongs to another change that is postgres and mssql only).
CREATE TABLE IF NOT EXISTS registered_plugins (
  plugin_name   varchar(128) NOT NULL,
  first_seen_at timestamp(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  last_seen_at  timestamp(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (plugin_name),
  CONSTRAINT ck_registered_plugins_name CHECK (CHAR_LENGTH(plugin_name) BETWEEN 1 AND 128)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
