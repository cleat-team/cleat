-- cleat#3600 (owner D52): which plugin NAMES some worker has registered, at least once. See
-- migrations/postgres/008 for the full reasoning; this is the same change.
--
-- MySQL has no schemas, so the table is unprefixed, like workers. CREATE TABLE IF NOT EXISTS is
-- already safe to run twice, so no guard procedure is needed. MySQL has no 005, 006 or 007: the
-- numbers are shared across dialects but a change that has no MySQL counterpart takes no MySQL
-- file, and the highest MySQL migration before this is 004.
--
-- plugin_name is compared BYTE FOR BYTE (utf8mb4_bin). The default collation folds case, so
-- "Slack-Notify" would match a registered "slack-notify", while a worker's own claim-time check
-- (checkPluginDeps) is an exact map lookup and would refuse it: the deploy check must say what
-- the worker will say. PostgreSQL's text is already exact.
CREATE TABLE IF NOT EXISTS registered_plugins (
  plugin_name   varchar(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  first_seen_at timestamp(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  last_seen_at  timestamp(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (plugin_name),
  CONSTRAINT ck_registered_plugins_name CHECK (CHAR_LENGTH(plugin_name) BETWEEN 1 AND 128)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- cleat#3600: one row for every use of the explicit override. See migrations/postgres/008 for the
-- full reasoning; this is the same change. CREATE TABLE IF NOT EXISTS is already safe to run twice.
-- MySQL has no per-table privilege model here, so append-only is the comment and the test.
CREATE TABLE IF NOT EXISTS plugin_dep_overrides (
  override_id        varchar(64) NOT NULL,
  occurred_at        timestamp(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  tenant_id          varchar(64) NOT NULL,
  definition_name    varchar(255) NOT NULL,
  definition_version int NOT NULL,
  via                varchar(64) NOT NULL,
  actor              varchar(255) NOT NULL DEFAULT '',
  missing_plugins    text NOT NULL,
  PRIMARY KEY (override_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
