-- cleat#2203. MySQL has had one login per database since the baseline: the
-- migrate step and the serving worker both run as the same user, so a worker
-- can INSERT/UPDATE/DELETE deployment_secrets even though nothing it does is
-- supposed to write there (docs/how-to/use-deployment-secrets.md says so
-- explicitly: "MySQL and SQL Server have no equivalent role split to revoke
-- from"). PostgreSQL's cleat_app (migrations/postgres/001_schema.sql, folded
-- in from the pre-compaction 005_app_role.sql) is the model this follows:
-- a least-privilege login the worker serves as, with the owner/migrate login
-- kept for schema changes only.
--
-- MySQL has no NOLOGIN role attribute the way PostgreSQL does, so the
-- equivalent of "exists, cannot authenticate until a deployment sets a
-- password" is ACCOUNT LOCK: a locked account refuses every authentication
-- attempt (ER_ACCOUNT_HAS_BEEN_LOCKED), independent of whatever password
-- hash it carries. CREATE USER without IDENTIFIED BY leaves no password set
-- at all, which is exactly the "the password is not this file's to know"
-- property cleat_app's PostgreSQL comment states; ACCOUNT LOCK is what keeps
-- that from being an open login in the meantime. A deployment unlocks it and
-- sets a real password together:
--
--   ALTER USER 'cleat_app'@'%' IDENTIFIED BY '<password>' ACCOUNT UNLOCK;
--
-- The host is '%' because the worker, like the owner login, connects over
-- the network rather than through a local socket -- the same host pattern
-- MYSQL_USER already gets from the image's own entrypoint.
CREATE USER IF NOT EXISTS 'cleat_app'@'%' ACCOUNT LOCK;

-- Every table gets SELECT -- `tbl_name` with no schema resolves against the
-- CONNECTION's default database, the same way the CREATE TABLE statements in
-- 001_schema.sql do, so this file never has to name the database.
-- deployment_secrets is included here (read is fine; see below for why write
-- is not) and is not repeated in the write grants that follow.
GRANT SELECT ON concurrency_keys TO 'cleat_app'@'%';
GRANT SELECT ON deployment_secrets TO 'cleat_app'@'%';
GRANT SELECT ON event_history TO 'cleat_app'@'%';
GRANT SELECT ON idempotency_keys TO 'cleat_app'@'%';
GRANT SELECT ON orgs TO 'cleat_app'@'%';
GRANT SELECT ON plugin_defs TO 'cleat_app'@'%';
GRANT SELECT ON plugin_tables TO 'cleat_app'@'%';
GRANT SELECT ON queue_holders TO 'cleat_app'@'%';
GRANT SELECT ON queue_rate_tokens TO 'cleat_app'@'%';
GRANT SELECT ON queues TO 'cleat_app'@'%';
GRANT SELECT ON slack_workspace TO 'cleat_app'@'%';
GRANT SELECT ON tenant_api_keys TO 'cleat_app'@'%';
GRANT SELECT ON tenant_domains TO 'cleat_app'@'%';
GRANT SELECT ON tenant_egress_allow TO 'cleat_app'@'%';
GRANT SELECT ON tenant_roles TO 'cleat_app'@'%';
GRANT SELECT ON tenant_secrets TO 'cleat_app'@'%';
GRANT SELECT ON tenant_settings TO 'cleat_app'@'%';
GRANT SELECT ON tenants TO 'cleat_app'@'%';
GRANT SELECT ON workers TO 'cleat_app'@'%';
GRANT SELECT ON workflow_defs TO 'cleat_app'@'%';
GRANT SELECT ON workflow_instances TO 'cleat_app'@'%';
GRANT SELECT ON workflow_memory_samples TO 'cleat_app'@'%';
GRANT SELECT ON workflow_memory_stats TO 'cleat_app'@'%';
GRANT SELECT ON workflow_promises TO 'cleat_app'@'%';
GRANT SELECT ON workflow_routing TO 'cleat_app'@'%';
GRANT SELECT ON workflow_schedules TO 'cleat_app'@'%';
GRANT SELECT ON workflow_signals TO 'cleat_app'@'%';
GRANT SELECT ON workflow_tags TO 'cleat_app'@'%';
GRANT SELECT ON workflow_update_requests TO 'cleat_app'@'%';
GRANT SELECT ON schema_migrations TO 'cleat_app'@'%';

-- INSERT/UPDATE/DELETE go to every table the worker actually writes EXCEPT
-- deployment_secrets. Values go in and are retired through cleatctl
-- (set-deployment-secret, retire-deployment-secret, reseal-deployment-secrets),
-- run with the owner/migrate login -- see docs/how-to/use-deployment-secrets.md.
-- There is deliberately no GRANT INSERT/UPDATE/DELETE on deployment_secrets
-- anywhere in this file: MySQL privilege checks are an OR across whatever
-- grant tables apply (global, database, table), with no DENY to override a
-- broader one, so the only way to keep this table out of a GRANT ... ON
-- `db`.* statement is to never issue one and grant every other table
-- individually instead, which is what this file does throughout.
GRANT INSERT, UPDATE, DELETE ON concurrency_keys TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON event_history TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON idempotency_keys TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON orgs TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON plugin_defs TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON plugin_tables TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON queue_holders TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON queue_rate_tokens TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON queues TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON slack_workspace TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON tenant_api_keys TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON tenant_domains TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON tenant_egress_allow TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON tenant_roles TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON tenant_secrets TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON tenant_settings TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON tenants TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workers TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workflow_defs TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workflow_instances TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workflow_memory_samples TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workflow_memory_stats TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workflow_promises TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workflow_routing TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workflow_schedules TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workflow_signals TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workflow_tags TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON workflow_update_requests TO 'cleat_app'@'%';
GRANT INSERT, UPDATE, DELETE ON schema_migrations TO 'cleat_app'@'%';

-- finalize_workflow_status is CALLed on every workflow completion
-- (engine/mysql_store.go); without EXECUTE the worker cannot finish a single
-- run as cleat_app.
GRANT EXECUTE ON PROCEDURE finalize_workflow_status TO 'cleat_app'@'%';

FLUSH PRIVILEGES;
