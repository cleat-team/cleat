-- cleat migration 039 (mysql): per-tenant execution limits.
--
-- IMPROVEMENT-PLAN 3.94 step 3, and step 2's option C: the settings table
-- ships on all three dialects with one uniform API, and on MySQL it holds the
-- single tenant's row -- the deployment's settings. No dialect-specific API,
-- no MySQL multi-tenancy work.
--
-- ---------------------------------------------------------------------------
-- There is no row-level security here and none is needed, which is a
-- consequence rather than an omission.
--
-- tiers.yaml D1 says MySQL is single-tenant, and since
-- 038_single_tenant_guard.sql that is enforced at the point of creation rather
-- than by breakage later -- a second tenant is refused by a unique key whose
-- name states the rule. A table that can only ever hold one tenant's row has
-- no cross-tenant read to prevent. The Postgres and SQL Server versions of
-- this table carry a policy because on those dialects the premise does not
-- hold.
--
-- ---------------------------------------------------------------------------
-- NULL means "no override", and that is not the same as zero.
--
-- Zero is already meaningful for each of these -- the executor tests
-- `ceiling > 0` before applying a bound, so zero means UNBOUNDED. Storing an
-- absent override as 0 would make "leave this to the operator" inexpressible,
-- and a row written to set one field would silently unbound the other two.
--
-- The CHECK constraints are a privilege boundary rather than input validation.
-- A tenant may only ever TIGHTEN a limit, because resolution clamps its value
-- to the operator flag. That property needs the stored value to be positive:
-- allow 0 and a tenant writes 0, resolution reads "unbounded", and the clamp
-- hands back MORE than the operator granted. MySQL enforces CHECK from
-- 8.0.16 and silently ignores it before that, so
-- migration/tenant_settings_check_test.go asserts the refusal rather than
-- trusting the version.
--
-- Deletion is by foreign key. ON DELETE CASCADE cannot be forgotten the way a
-- line in a hand-enumerated cleanup routine can.
--
-- Note for anyone editing this file: no comment here may contain a semicolon.
-- migration/runner.go splits MySQL migrations on the semicolon character with
-- no comment awareness. See IMPROVEMENT-PLAN 3.13.

CREATE TABLE IF NOT EXISTS tenant_settings (
    tenant_id                  CHAR(36) NOT NULL,
    wasm_instance_timeout_ms   BIGINT,
    wasm_wall_clock_ceiling_ms BIGINT,
    host_retry_budget_ms       BIGINT,
    updated_at                 TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (tenant_id),
    CONSTRAINT fk_tenant_settings_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants(tenant_id) ON DELETE CASCADE,
    CONSTRAINT ck_tenant_settings_instance_timeout_positive
        CHECK (wasm_instance_timeout_ms IS NULL OR wasm_instance_timeout_ms > 0),
    CONSTRAINT ck_tenant_settings_wall_clock_positive
        CHECK (wasm_wall_clock_ceiling_ms IS NULL OR wasm_wall_clock_ceiling_ms > 0),
    CONSTRAINT ck_tenant_settings_retry_budget_positive
        CHECK (host_retry_budget_ms IS NULL OR host_retry_budget_ms > 0)
) ENGINE=InnoDB;
