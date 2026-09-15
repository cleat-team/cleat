-- Caller-controlled JSON is stored as TEXT on MySQL, as it already is on SQL Server.
--
-- cleat#1022. MySQL's JSON type keeps an integer as INT64 or UINT64 and falls
-- back to DOUBLE when it fits neither, so a value outside [-2^63, 2^64-1] --
-- and any decimal needing more precision than a float64 holds -- is REWRITTEN
-- on the way in. Nothing errors. The value is still valid JSON, still an
-- object, still the right shape, and still plausible:
--
--     sent    {"x":123456789012345678901234567890}
--     stored  {"x": 1.2345678901234566e29}
--
-- The narrowing belongs to the JSON *type*, not to any one column: the same
-- INSERT into a TEXT column in the same row preserves the digits exactly. So
-- every JSON column carrying a caller's value has it, and `result` -- the one
-- cleat#1022 was filed about -- was simply the one somebody looked at.
--
-- WHY TEXT IS THE FIX RATHER THAN A DOCUMENTED LIMIT OR A LOG LINE.
-- SQL Server already does exactly this and preserves every value above:
--
--     input NVARCHAR(MAX) NOT NULL DEFAULT '{}',
--     CONSTRAINT ck_workflow_instances_input CHECK (ISJSON(input) = 1)
--
-- This migration is that design ported column for column, including the
-- constraint names. It is not a new idea; it is the approach one shipped
-- backend already uses, applied to the backend that lacks it. After it, all
-- three dialects preserve, rather than two agreeing and MySQL being documented
-- as lesser.
--
-- WHY THE CHECK CONSTRAINT IS NOT OPTIONAL. Dropping to LONGTEXT gives up the
-- JSON type's validation, and invalid JSON would become storable where today
-- the column refuses it. JSON_VALID restores exactly that and nothing else.
-- Note the hazard: CHECK is parsed and IGNORED before MySQL 8.0.16, so on an
-- old server these constraints are decoration. That is a pre-existing
-- dependency rather than a new one -- migrations/mysql/036 relies on it for
-- tenant_settings, and engine/mysql_tenant_settings_test.go tests it at runtime
-- for this reason.
--
-- WHAT IS DELIBERATELY NOT HERE:
--
--   * workflow_instances.query_state stays JSON. It is the ONE column MySQL
--     genuinely queries as JSON -- JSON_UNQUOTE(JSON_EXTRACT(query_state, ?))
--     at engine/mysql_store.go:434 -- so it cannot become text without
--     rewriting that read. It therefore remains degraded on MySQL, which is
--     stated rather than left implied.
--   * compaction_state, plugin_vers, allowed_signals, workflow_defs.*, and
--     plugin_defs.config are engine- or operator-controlled structure, not a
--     caller's value round-tripping through the system.
--   * idempotency_keys.result is NOT here even though SQL Server still carries
--     a ck_idempotency_keys_result. MySQL has no such column: migration
--     054_idempotency_keys_result_had_no_reader.sql dropped it. The scope for
--     this migration was first derived by reading 001_schema.sql and was wrong
--     by exactly this one column -- the authoritative source is the LIVE
--     schema (information_schema.COLUMNS WHERE DATA_TYPE='json'), because a
--     later migration can add or drop a column and 001 never changes.
--   * Plugin tables are out of scope and are a separate migration system.
--     Several of them do hold caller data in JSON columns on MySQL and have
--     the same defect -- kv_store.value, webhook_events.payload,
--     eventstore's event, scheduler's input. Filed separately rather than
--     silently widened here as cleat#1622. No count is given on purpose;
--     re-derive it:
--
--       git ls-files 'plugins/*' | grep -E '\.(go|sql)$' |
--         xargs grep -nE '^[[:space:]]*`?[a-z_]+`? +JSON\b'
--
--     blobstore.tags must STAY json whatever is decided: it is queried with
--     JSON_CONTAINS (plugins/blobstore/queries.go:100).
--
-- Existing rows are unaffected and already-degraded values stay as they are:
-- there is nothing to recover, the digits were lost at write time. What changes
-- is every write from here on.

ALTER TABLE workflow_instances
    MODIFY input  LONGTEXT NOT NULL DEFAULT ('{}'),
    MODIFY result LONGTEXT NULL;

ALTER TABLE workflow_instances
    ADD CONSTRAINT ck_workflow_instances_input  CHECK (JSON_VALID(input)),
    ADD CONSTRAINT ck_workflow_instances_result CHECK (result IS NULL OR JSON_VALID(result));

ALTER TABLE event_history
    MODIFY payload LONGTEXT NULL;

ALTER TABLE event_history
    ADD CONSTRAINT ck_event_history_payload CHECK (payload IS NULL OR JSON_VALID(payload));

ALTER TABLE workflow_signals
    MODIFY payload LONGTEXT NOT NULL DEFAULT ('{}');

ALTER TABLE workflow_signals
    ADD CONSTRAINT ck_workflow_signals_payload CHECK (JSON_VALID(payload));

ALTER TABLE workflow_promises
    MODIFY result LONGTEXT NULL;

ALTER TABLE workflow_promises
    ADD CONSTRAINT ck_workflow_promises_result CHECK (result IS NULL OR JSON_VALID(result));

ALTER TABLE workflow_schedules
    MODIFY input LONGTEXT NOT NULL DEFAULT ('{}');

ALTER TABLE workflow_schedules
    ADD CONSTRAINT ck_workflow_schedules_input CHECK (JSON_VALID(input));

ALTER TABLE workflow_update_requests
    MODIFY payload LONGTEXT NOT NULL DEFAULT ('{}'),
    MODIFY result  LONGTEXT NULL;

ALTER TABLE workflow_update_requests
    ADD CONSTRAINT ck_workflow_update_requests_payload CHECK (JSON_VALID(payload)),
    ADD CONSTRAINT ck_workflow_update_requests_result  CHECK (result IS NULL OR JSON_VALID(result));
