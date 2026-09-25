--
-- Name: claim_workflows(text, text[], integer); Type: FUNCTION; Schema: admin; Owner: cleat_dispatcher
--

CREATE FUNCTION admin.claim_workflows(p_worker_id text, p_task_queues text[], p_limit integer) RETURNS TABLE(id text, def_name text, def_version integer, status text, input jsonb, assigned_to text, next_wake_at timestamp with time zone, tenant_id uuid, created_at timestamp with time zone, error_code text, error_op text, generation bigint, priority integer, trace_id text, pending_terminal_status text)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public', 'pg_temp'
    AS $$
    WITH candidates AS (
        SELECT w.id FROM workflow_instances w
        WHERE w.status IN ('ready', 'terminating')
          AND w.next_wake_at <= now()
          AND w.task_queue = ANY(p_task_queues)
        ORDER BY w.priority ASC, w.created_at
        LIMIT p_limit
        FOR UPDATE SKIP LOCKED
    )
    UPDATE workflow_instances w
    SET status = 'running',
        assigned_to = p_worker_id,
        heartbeat_at = now(),
        started_at = COALESCE(w.started_at, now()),
        generation = w.generation + 1
    FROM candidates c
    WHERE w.id = c.id
    RETURNING w.id, w.def_name, w.def_version, w.status, w.input, w.assigned_to,
              w.next_wake_at, w.tenant_id, w.created_at, w.error_code, w.error_op,
              w.generation, COALESCE(w.priority, 0), COALESCE(w.trace_id, ''),
              COALESCE(w.pending_terminal_status, '');
$$;


ALTER FUNCTION admin.claim_workflows(p_worker_id text, p_task_queues text[], p_limit integer) OWNER TO cleat_dispatcher;

--
-- Name: create_tenant_role(uuid, text); Type: FUNCTION; Schema: admin; Owner: postgres
--

CREATE FUNCTION admin.create_tenant_role(p_tenant_id uuid, p_password text) RETURNS text
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'public', 'pg_temp'
    AS $$
DECLARE
    v_role_name TEXT;
BEGIN
    IF p_password IS NULL OR length(p_password) < 32 THEN
        RAISE EXCEPTION 'create_tenant_role: password must be at least 32 '
            'characters (got %); derive it with plugin.TenantRolePassword',
            coalesce(length(p_password), 0);
    END IF;

    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        BEGIN
    IF p_password IS NULL OR length(p_password) < 32 THEN
        RAISE EXCEPTION 'create_tenant_role: password must be at least 32 '
            'characters (got %); derive it with plugin.TenantRolePassword',
            coalesce(length(p_password), 0);
    END IF;

            EXECUTE format(
                'CREATE ROLE %I WITH LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT CONNECTION LIMIT 10',
                v_role_name, p_password
            );
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'create_tenant_role: cannot create role % (SQLSTATE: %) -- skipping (single-tenant mode)', v_role_name, SQLSTATE;
            RETURN NULL;
        END;
    ELSE
        BEGIN
    IF p_password IS NULL OR length(p_password) < 32 THEN
        RAISE EXCEPTION 'create_tenant_role: password must be at least 32 '
            'characters (got %); derive it with plugin.TenantRolePassword',
            coalesce(length(p_password), 0);
    END IF;

            EXECUTE format('ALTER ROLE %I WITH PASSWORD %L', v_role_name, p_password);
        EXCEPTION WHEN OTHERS THEN
            RAISE WARNING 'create_tenant_role: cannot alter password for role % (SQLSTATE: %)', v_role_name, SQLSTATE;
        END;
    END IF;

    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I AUTHORIZATION %I',
        'tenant_' || replace(p_tenant_id::text, '-', '_'), v_role_name);

    -- current_schema() rather than a literal: the schema cleat's tables live
    -- in is --schema's to choose (cleat#1287). Asking rather than stating is
    -- also what makes this work under every applier of these files, none of
    -- which substitutes anything.
    EXECUTE format('ALTER ROLE %I SET search_path = %L, %I', v_role_name,
        'tenant_' || replace(p_tenant_id::text, '-', '_'), current_schema());
    EXECUTE format('ALTER ROLE %I SET cleat.tenant_id = %L', v_role_name, p_tenant_id);

    EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', current_schema(), v_role_name);
    EXECUTE format('GRANT USAGE ON SCHEMA admin TO %I', v_role_name);

    PERFORM admin.grant_core_tables_to_tenant_role(v_role_name);

    -- role_name only; the password column is dropped at the end of this file.
    INSERT INTO admin.tenant_roles (tenant_id, role_name)
    VALUES (p_tenant_id, v_role_name)
    ON CONFLICT (tenant_id) DO UPDATE SET role_name = EXCLUDED.role_name;

    RETURN v_role_name;
END;
$$;


ALTER FUNCTION admin.create_tenant_role(p_tenant_id uuid, p_password text) OWNER TO postgres;

--
-- Name: drop_tenant(uuid, text); Type: FUNCTION; Schema: admin; Owner: postgres
--

CREATE FUNCTION admin.drop_tenant(p_tenant_id uuid, p_schema text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $_$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_plugin_table RECORD;
    v_deleted BIGINT;
    v_core_table TEXT;
BEGIN
    IF p_schema IS NULL OR p_schema = '' THEN
        RAISE EXCEPTION 'admin.drop_tenant: p_schema is required -- it names the schema holding this deployment''s cleat tables, and must match the --schema cleat-worker was started with. Passing it is what stops the DELETEs resolving through the caller''s search_path (cleat#1363)';
    END IF;

    IF p_tenant_id = '00000000-0000-0000-0000-000000000000' THEN
        RAISE EXCEPTION 'admin.drop_tenant: refusing to delete the default tenant (00000000-0000-0000-0000-000000000000) -- it is shared by every single-tenant deployment and by workflow_defs/plugin_defs, which are not tenant-owned data';
    END IF;

    IF to_regnamespace(p_schema) IS NULL THEN
        RAISE EXCEPTION 'admin.drop_tenant: schema % does not exist. Deleting nothing and reporting success is the failure this function was fixed to stop, so it refuses instead', p_schema;
    END IF;

    -- Makes every DELETE below correct regardless of whether the owning or
    -- executing role is a superuser.
    PERFORM set_config('cleat.tenant_id', p_tenant_id::text, true);

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');
    v_role_name := 'cleat_tenant_' || replace(p_tenant_id::text, '-', '_');

    -- The seven core tables, in dependency order, each schema-qualified from
    -- p_schema. The order is 066's and is load bearing:
    --
    --   event_history       no FK/CASCADE back to workflow_instances (dropped
    --                       deliberately by 003_procedures.sql), so it must go
    --                       first or it is orphaned.
    --   workflow_instances  cascades workflow_signals, workflow_promises,
    --                       concurrency_keys and workflow_update_requests via
    --                       real ON DELETE CASCADE FKs.
    --   workflow_schedules  no FK to workflow_instances; own tenant_id.
    --   workflow_tags       FK to workflow_defs (NO ACTION), own tenant_id.
    --   workflow_routing    as workflow_tags.
    --   idempotency_keys    own tenant_id (010); result/error_msg hold a
    --                       workflow's actual output.
    --   workflow_defs       LAST of the seven: workflow_instances carries an FK
    --                       to it (NO ACTION), so it cannot be deleted while an
    --                       instance references it. By here there are none.
    --
    -- plugin_defs is deliberately absent: it has no tenant_id column at all
    -- (PRIMARY KEY (name, version)) and is genuinely shared.
    FOREACH v_core_table IN ARRAY ARRAY[
        'event_history',
        'workflow_instances',
        'workflow_schedules',
        'workflow_tags',
        'workflow_routing',
        'idempotency_keys',
        -- cleat#1644. Both carry tenant_id since 056 and neither has a foreign
        -- key to anything, so nothing cascaded them either: a dropped tenant's
        -- memory profile -- the names of the workflows it ran and how much
        -- memory each used -- survived indefinitely. Position in this array is
        -- free for exactly that reason; they are here rather than at the end so
        -- that workflow_defs stays visibly last.
        'workflow_memory_samples',
        'workflow_memory_stats',
        'workflow_defs'
    ] LOOP
        IF to_regclass(format('%I.%I', p_schema, v_core_table)) IS NULL THEN
            RAISE EXCEPTION 'admin.drop_tenant: %.% does not exist. Either p_schema is wrong or this deployment is not fully migrated; either way, continuing would delete some of this tenant''s data and report success', p_schema, v_core_table;
        END IF;
        EXECUTE format('DELETE FROM %I.%I WHERE tenant_id = $1', p_schema, v_core_table)
            USING p_tenant_id;
        GET DIAGNOSTICS v_deleted = ROW_COUNT;
        RAISE DEBUG 'admin.drop_tenant: deleted % row(s) from %.%',
            v_deleted, p_schema, v_core_table;
    END LOOP;

    -- cleat#1289. Every table a plugin declared TenantScoped, which is the same
    -- declaration that gave it a row-level security policy. Schema-qualified
    -- from the registry, which is where a plugin table's schema is recorded --
    -- NOT from p_schema, because a plugin table need not live in the same
    -- schema as the core tables.
    FOR v_plugin_table IN
        SELECT schema_name, table_name
        FROM admin.plugin_tables
        WHERE tenant_scoped
        ORDER BY schema_name, table_name
    LOOP
        -- A registry row can outlive its table: a plugin is removed from the
        -- build, or its migration is reversed, and nothing deletes the row.
        -- Without this guard the first such row makes admin.drop_tenant raise
        -- and TENANT DELETION STOPS WORKING ENTIRELY, for every tenant.
        --
        -- A warning rather than silence, because the other reading of a missing
        -- table is a registration that recorded the wrong schema -- in which
        -- case rows DO survive, and the skip is the only evidence.
        IF to_regclass(format('%I.%I', v_plugin_table.schema_name,
                              v_plugin_table.table_name)) IS NULL THEN
            RAISE WARNING 'admin.drop_tenant: admin.plugin_tables names %.%, which does not exist -- skipping. If that table does exist under another schema, this tenant''s rows in it are NOT deleted.',
                v_plugin_table.schema_name, v_plugin_table.table_name;
            CONTINUE;
        END IF;

        EXECUTE format('DELETE FROM %I.%I WHERE tenant_id = $1',
                       v_plugin_table.schema_name, v_plugin_table.table_name)
            USING p_tenant_id;
        GET DIAGNOSTICS v_deleted = ROW_COUNT;
        RAISE DEBUG 'admin.drop_tenant: deleted % row(s) from %.%',
            v_deleted, v_plugin_table.schema_name, v_plugin_table.table_name;
    END LOOP;

    -- Must precede the admin.tenants delete: admin.tenant_api_keys' FK to
    -- admin.tenants has no ON DELETE clause (NO ACTION), so deleting
    -- admin.tenants first fails for any tenant that ever had a key issued.
    DELETE FROM admin.tenant_api_keys WHERE tenant_id = p_tenant_id;

    -- DROP ROLE refuses to drop a role that still holds privileges anywhere,
    -- and that error aborts the whole function -- rolling back every DELETE
    -- above with it, since a single CALL is one transaction. DROP OWNED BY
    -- strips the grants first. Guarded on existence because DROP OWNED BY has
    -- no IF EXISTS form and the role may legitimately not exist (single-tenant
    -- mode warns and skips role creation).
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role_name) THEN
        EXECUTE format('DROP OWNED BY %I', v_role_name);
    END IF;
    EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v_schema_name);
    EXECUTE format('DROP ROLE IF EXISTS %I', v_role_name);

    DELETE FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    DELETE FROM admin.tenants WHERE tenant_id = p_tenant_id;
END;
$_$;


ALTER FUNCTION admin.drop_tenant(p_tenant_id uuid, p_schema text) OWNER TO postgres;

--
-- Name: get_due_schedules(); Type: FUNCTION; Schema: admin; Owner: cleat_dispatcher
--

CREATE FUNCTION admin.get_due_schedules() RETURNS TABLE(name text, def_name text, entry_point text, cron_expression text, input jsonb, disabled_at timestamp with time zone, next_run_at timestamp with time zone, last_run_at timestamp with time zone, timezone text, tenant_id uuid, misfire_policy text, catch_up_limit integer, overlap_policy text, last_run_id text)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public', 'pg_temp'
    AS $$
    SELECT s.name, s.def_name, s.entry_point, s.cron_expression, s.input,
           s.disabled_at, s.next_run_at, s.last_run_at, s.timezone, s.tenant_id,
           s.misfire_policy, s.catch_up_limit, s.overlap_policy,
           COALESCE(s.last_run_id, '')
    FROM workflow_schedules s
    WHERE s.disabled_at IS NULL
      AND s.next_run_at <= now()
    ORDER BY s.next_run_at;
$$;


ALTER FUNCTION admin.get_due_schedules() OWNER TO cleat_dispatcher;

--
-- Name: grant_core_tables_to_tenant_role(text); Type: FUNCTION; Schema: admin; Owner: postgres
--

CREATE FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name text) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'public', 'pg_temp'
    AS $$
DECLARE
    i int;
BEGIN
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_defs TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_instances TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.event_history TO %I', current_schema(), p_role_name);
    -- cleat#2059. A GRANT on a partitioned parent does not reach its
    -- partitions (measured in docs/schema-partitioning-design.md, "Grants:
    -- Per-relation") -- so every tenant role also needs the grant repeated
    -- on each of event_history's 64 hash partitions by name.
    FOR i IN 0..63 LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.event_history_p%s TO %I', current_schema(), i, p_role_name);
    END LOOP;
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_signals TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_schedules TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_promises TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.idempotency_keys TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.concurrency_keys TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_update_requests TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.workflow_tags TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT ON %I.tenant_settings TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.queues TO %I', current_schema(), p_role_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.queue_holders TO %I', current_schema(), p_role_name);

    -- cleat#1918. The rate-limit counter table; granted beside queue_holders,
    -- its nearest sibling in shape.
    EXECUTE format('GRANT SELECT, INSERT, DELETE ON %I.queue_rate_tokens TO %I', current_schema(), p_role_name);

    EXECUTE format('GRANT USAGE ON ALL SEQUENCES IN SCHEMA %I TO %I', current_schema(), p_role_name);
END;
$$;


ALTER FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name text) OWNER TO postgres;

--
-- Name: grant_plugin_to_tenant(text, uuid); Type: FUNCTION; Schema: admin; Owner: postgres
--

CREATE FUNCTION admin.grant_plugin_to_tenant(p_plugin_name text, p_tenant_id uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_table_name TEXT;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'grant_plugin_to_tenant: no role for tenant % -- skipping (single-tenant mode)', p_tenant_id;
        RETURN;
    END IF;

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');

    FOR v_table_name IN
        SELECT t.table_name FROM admin.plugin_tables t WHERE t.plugin_name = p_plugin_name
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.%I TO %I',
            v_schema_name, v_table_name, v_role_name);
    END LOOP;
END;
$$;


ALTER FUNCTION admin.grant_plugin_to_tenant(p_plugin_name text, p_tenant_id uuid) OWNER TO postgres;

--
-- Name: in_flight_workflow_ids(); Type: FUNCTION; Schema: admin; Owner: cleat_dispatcher
--

CREATE FUNCTION admin.in_flight_workflow_ids() RETURNS TABLE(id text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'public', 'pg_temp'
    AS $$
    SELECT w.id FROM workflow_instances w WHERE w.status IN ('ready', 'running');
$$;


ALTER FUNCTION admin.in_flight_workflow_ids() OWNER TO cleat_dispatcher;

--
-- Name: revoke_plugin_from_tenant(text, uuid); Type: FUNCTION; Schema: admin; Owner: postgres
--

CREATE FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name text, p_tenant_id uuid) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE
    v_role_name TEXT;
    v_schema_name TEXT;
    v_table_name TEXT;
BEGIN
    SELECT role_name INTO v_role_name FROM admin.tenant_roles WHERE tenant_id = p_tenant_id;
    IF v_role_name IS NULL THEN
        RAISE WARNING 'revoke_plugin_from_tenant: no role for tenant % -- skipping (single-tenant mode)', p_tenant_id;
        RETURN;
    END IF;

    v_schema_name := 'tenant_' || replace(p_tenant_id::text, '-', '_');

    FOR v_table_name IN
        SELECT t.table_name FROM admin.plugin_tables t WHERE t.plugin_name = p_plugin_name
    LOOP
        EXECUTE format('REVOKE ALL ON %I.%I FROM %I',
            v_schema_name, v_table_name, v_role_name);
    END LOOP;
END;
$$;


ALTER FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name text, p_tenant_id uuid) OWNER TO postgres;

--
-- Name: tenants_org_id_is_immutable(); Type: FUNCTION; Schema: admin; Owner: postgres
--

CREATE FUNCTION admin.tenants_org_id_is_immutable() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.org_id IS DISTINCT FROM OLD.org_id THEN
        RAISE EXCEPTION 'admin.tenants.org_id is immutable and cannot be changed (tenant_id=%, from org %  to org %)',
            OLD.tenant_id, OLD.org_id, NEW.org_id
            USING ERRCODE = '23514'; -- check_violation, the same code a CHECK constraint would raise
    END IF;
    RETURN NEW;
END;
$$;


ALTER FUNCTION admin.tenants_org_id_is_immutable() OWNER TO postgres;

--
-- Name: tenant_row_is_visible(uuid); Type: FUNCTION; Schema: cleat; Owner: postgres
--

CREATE FUNCTION cleat.tenant_row_is_visible(row_tenant uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE
        WHEN coalesce(current_setting('cleat.cross_tenant', true), '') <> ''
            THEN true
        ELSE row_tenant = cleat.assert_tenant_set()
    END
$$;


ALTER FUNCTION cleat.tenant_row_is_visible(row_tenant uuid) OWNER TO postgres;

--
-- Name: batch_flush_events(jsonb); Type: FUNCTION; Schema: public; Owner: postgres
--

CREATE FUNCTION public.batch_flush_events(p_events jsonb) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO event_history (
        workflow_id, step, event_type, service, operation,
        request, response, error, duration_ms, signal_names,
        timeout_ms, signal_name, signal_payload, defer_description,
        defer_id, child_name, child_input, run_id, new_input,
        plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
        promise_name, promise_id, promise_result, promise_error,
        payload, created_at, checksum, tenant_id
    )
    SELECT
        workflow_id, step, event_type, service, operation,
        request, response, error, duration_ms, signal_names,
        timeout_ms, signal_name, signal_payload, defer_description,
        defer_id, child_name, child_input, run_id, new_input,
        plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
        promise_name, promise_id, promise_result, promise_error,
        payload, created_at, checksum, tenant_id
    FROM jsonb_populate_recordset(NULL::event_history, p_events)
    ON CONFLICT (workflow_id, step) DO UPDATE
        SET response = EXCLUDED.response, error = EXCLUDED.error
        WHERE event_history.response = '' AND event_history.error IS NULL;
END;
$$;


ALTER FUNCTION public.batch_flush_events(p_events jsonb) OWNER TO postgres;

--
-- Name: finalize_workflow_status(text, text, bigint, text, text, text, text, jsonb, timestamp with time zone, text); Type: FUNCTION; Schema: public; Owner: postgres
--

CREATE FUNCTION public.finalize_workflow_status(p_workflow_id text, p_worker_id text, p_generation bigint, p_final_status text, p_result text, p_error_code text, p_error_op text, p_query_state jsonb, p_next_wake_at timestamp with time zone, p_notify_channel text) RETURNS boolean
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_rows_updated INT;
BEGIN
    -- Update workflow status, fenced on (assigned_to, generation) so a
    -- caller that no longer owns the workflow cannot modify it.
    CASE p_final_status
        WHEN 'done' THEN
            UPDATE workflow_instances
            SET status = 'done',
                result = p_result::jsonb,
                completed_at = now(),
                completed_by = assigned_to,
                assigned_to = NULL,
                query_state = p_query_state
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation;

        WHEN 'ready' THEN
            UPDATE workflow_instances
            SET status = 'ready',
                assigned_to = NULL,
                next_wake_at = CASE
                                    WHEN signal_seq <> signal_seq_at_claim
                                      OR signal_consumed_seq <> signal_consumed_at_claim
                                    THEN now() ELSE p_next_wake_at END,
                query_state = p_query_state
            WHERE id = p_workflow_id
              AND assigned_to = p_worker_id
              AND generation = p_generation;

        ELSE
            RAISE EXCEPTION 'finalize_workflow_status: unknown final status: %', p_final_status;
    END CASE;

    GET DIAGNOSTICS v_rows_updated = ROW_COUNT;

    -- Terminal status side-effects -- only run if the fenced UPDATE above
    -- actually matched this caller's (worker_id, generation), and only for
    -- 'done': a real failure never reaches this procedure (cleat#1973), so
    -- there is no 'failed' case left to guard here.
    IF v_rows_updated > 0 AND p_final_status = 'done' THEN
        -- Wake parent workflow atomically.
        UPDATE workflow_instances
        SET next_wake_at = now()
        WHERE id = (
            SELECT parent_workflow_id FROM workflow_instances WHERE id = p_workflow_id
        )
        AND status IN ('ready', 'suspended');

        -- Delete this workflow's events -- they are no longer needed
        -- for replay once the workflow has reached a terminal state.
        -- This keeps event_history bounded to active workflows only,
        -- preventing unbounded table growth that slows per-step INSERTs.
        DELETE FROM event_history WHERE workflow_id = p_workflow_id;
    END IF;

    -- Dispatch hint
    IF p_notify_channel IS NOT NULL AND p_notify_channel != '' THEN
        PERFORM pg_notify(p_notify_channel, '');
    END IF;

    RETURN v_rows_updated > 0;
END;
$$;


ALTER FUNCTION public.finalize_workflow_status(p_workflow_id text, p_worker_id text, p_generation bigint, p_final_status text, p_result text, p_error_code text, p_error_op text, p_query_state jsonb, p_next_wake_at timestamp with time zone, p_notify_channel text) OWNER TO postgres;

--
-- Name: flush_event_step(text, integer, text, text, text, text, text, text, bigint, text, bigint, text, text, text, text, text, text, text, text, text, text, text, text, text, text, text, text, text, jsonb, text, uuid); Type: FUNCTION; Schema: public; Owner: postgres
--

CREATE FUNCTION public.flush_event_step(p_workflow_id text, p_step integer, p_event_type text, p_service text, p_operation text, p_request text, p_response text, p_error text, p_duration_ms bigint, p_signal_names text, p_timeout_ms bigint, p_signal_name text, p_signal_payload text, p_defer_description text, p_defer_id text, p_child_name text, p_child_input text, p_run_id text, p_new_input text, p_plugin_name text, p_plugin_func text, p_plugin_input text, p_plugin_output text, p_plugin_error text, p_promise_name text, p_promise_id text, p_promise_result text, p_promise_error text, p_payload jsonb, p_checksum text, p_tenant_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO event_history (
        workflow_id, step, event_type, service, operation,
        request, response, error, duration_ms, signal_names,
        timeout_ms, signal_name, signal_payload, defer_description,
        defer_id, child_name, child_input, run_id, new_input,
        plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
        promise_name, promise_id, promise_result, promise_error,
        payload, checksum, created_at, tenant_id
    ) VALUES (
        p_workflow_id, p_step, p_event_type,
        nullif(p_service, ''), nullif(p_operation, ''),
        nullif(p_request, ''), nullif(p_response, ''), nullif(p_error, ''),
        nullif(p_duration_ms, 0), nullif(p_signal_names, ''),
        nullif(p_timeout_ms, 0), nullif(p_signal_name, ''),
        nullif(p_signal_payload, ''), nullif(p_defer_description, ''),
        nullif(p_defer_id, ''), nullif(p_child_name, ''),
        nullif(p_child_input, ''), nullif(p_run_id, ''),
        nullif(p_new_input, ''), nullif(p_plugin_name, ''),
        nullif(p_plugin_func, ''), nullif(p_plugin_input, ''),
        nullif(p_plugin_output, ''), nullif(p_plugin_error, ''),
        nullif(p_promise_name, ''), nullif(p_promise_id, ''),
        nullif(p_promise_result, ''), nullif(p_promise_error, ''),
        CASE WHEN p_payload IS NOT NULL THEN p_payload ELSE NULL END,
        p_checksum, now(), p_tenant_id
    ) ON CONFLICT (workflow_id, step) DO UPDATE
        SET response = EXCLUDED.response,
            error = EXCLUDED.error
        WHERE event_history.response = ''
          AND event_history.error IS NULL;
END;
$$;


ALTER FUNCTION public.flush_event_step(p_workflow_id text, p_step integer, p_event_type text, p_service text, p_operation text, p_request text, p_response text, p_error text, p_duration_ms bigint, p_signal_names text, p_timeout_ms bigint, p_signal_name text, p_signal_payload text, p_defer_description text, p_defer_id text, p_child_name text, p_child_input text, p_run_id text, p_new_input text, p_plugin_name text, p_plugin_func text, p_plugin_input text, p_plugin_output text, p_plugin_error text, p_promise_name text, p_promise_id text, p_promise_result text, p_promise_error text, p_payload jsonb, p_checksum text, p_tenant_id uuid) OWNER TO postgres;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: tenants tenants_org_id_immutable; Type: TRIGGER; Schema: admin; Owner: postgres
--

CREATE TRIGGER tenants_org_id_immutable BEFORE UPDATE ON admin.tenants FOR EACH ROW EXECUTE FUNCTION admin.tenants_org_id_is_immutable();


--
-- Name: FUNCTION claim_workflows(p_worker_id text, p_task_queues text[], p_limit integer); Type: ACL; Schema: admin; Owner: cleat_dispatcher
--

REVOKE ALL ON FUNCTION admin.claim_workflows(p_worker_id text, p_task_queues text[], p_limit integer) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.claim_workflows(p_worker_id text, p_task_queues text[], p_limit integer) TO cleat_app;


--
-- Name: FUNCTION create_tenant_role(p_tenant_id uuid, p_password text); Type: ACL; Schema: admin; Owner: postgres
--

REVOKE ALL ON FUNCTION admin.create_tenant_role(p_tenant_id uuid, p_password text) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.create_tenant_role(p_tenant_id uuid, p_password text) TO cleat_app;


--
-- Name: FUNCTION drop_tenant(p_tenant_id uuid, p_schema text); Type: ACL; Schema: admin; Owner: postgres
--

REVOKE ALL ON FUNCTION admin.drop_tenant(p_tenant_id uuid, p_schema text) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.drop_tenant(p_tenant_id uuid, p_schema text) TO cleat_app;


--
-- Name: FUNCTION get_due_schedules(); Type: ACL; Schema: admin; Owner: cleat_dispatcher
--

REVOKE ALL ON FUNCTION admin.get_due_schedules() FROM PUBLIC;
GRANT ALL ON FUNCTION admin.get_due_schedules() TO cleat_app;


--
-- Name: FUNCTION grant_core_tables_to_tenant_role(p_role_name text); Type: ACL; Schema: admin; Owner: postgres
--

REVOKE ALL ON FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name text) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.grant_core_tables_to_tenant_role(p_role_name text) TO cleat_app;


--
-- Name: FUNCTION grant_plugin_to_tenant(p_plugin_name text, p_tenant_id uuid); Type: ACL; Schema: admin; Owner: postgres
--

REVOKE ALL ON FUNCTION admin.grant_plugin_to_tenant(p_plugin_name text, p_tenant_id uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.grant_plugin_to_tenant(p_plugin_name text, p_tenant_id uuid) TO cleat_app;


--
-- Name: FUNCTION in_flight_workflow_ids(); Type: ACL; Schema: admin; Owner: cleat_dispatcher
--

REVOKE ALL ON FUNCTION admin.in_flight_workflow_ids() FROM PUBLIC;
GRANT ALL ON FUNCTION admin.in_flight_workflow_ids() TO cleat_app;
GRANT ALL ON FUNCTION admin.in_flight_workflow_ids() TO cleat_sweep;


--
-- Name: FUNCTION revoke_plugin_from_tenant(p_plugin_name text, p_tenant_id uuid); Type: ACL; Schema: admin; Owner: postgres
--

REVOKE ALL ON FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name text, p_tenant_id uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION admin.revoke_plugin_from_tenant(p_plugin_name text, p_tenant_id uuid) TO cleat_app;


--
-- Name: FUNCTION tenants_org_id_is_immutable(); Type: ACL; Schema: admin; Owner: postgres
--

REVOKE ALL ON FUNCTION admin.tenants_org_id_is_immutable() FROM PUBLIC;
GRANT ALL ON FUNCTION admin.tenants_org_id_is_immutable() TO cleat_app;


--
-- Name: FUNCTION batch_flush_events(p_events jsonb); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.batch_flush_events(p_events jsonb) TO cleat_app;


--
-- Name: FUNCTION finalize_workflow_status(p_workflow_id text, p_worker_id text, p_generation bigint, p_final_status text, p_result text, p_error_code text, p_error_op text, p_query_state jsonb, p_next_wake_at timestamp with time zone, p_notify_channel text); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.finalize_workflow_status(p_workflow_id text, p_worker_id text, p_generation bigint, p_final_status text, p_result text, p_error_code text, p_error_op text, p_query_state jsonb, p_next_wake_at timestamp with time zone, p_notify_channel text) TO cleat_app;


--
-- Name: FUNCTION flush_event_step(p_workflow_id text, p_step integer, p_event_type text, p_service text, p_operation text, p_request text, p_response text, p_error text, p_duration_ms bigint, p_signal_names text, p_timeout_ms bigint, p_signal_name text, p_signal_payload text, p_defer_description text, p_defer_id text, p_child_name text, p_child_input text, p_run_id text, p_new_input text, p_plugin_name text, p_plugin_func text, p_plugin_input text, p_plugin_output text, p_plugin_error text, p_promise_name text, p_promise_id text, p_promise_result text, p_promise_error text, p_payload jsonb, p_checksum text, p_tenant_id uuid); Type: ACL; Schema: public; Owner: postgres
--

GRANT ALL ON FUNCTION public.flush_event_step(p_workflow_id text, p_step integer, p_event_type text, p_service text, p_operation text, p_request text, p_response text, p_error text, p_duration_ms bigint, p_signal_names text, p_timeout_ms bigint, p_signal_name text, p_signal_payload text, p_defer_description text, p_defer_id text, p_child_name text, p_child_input text, p_run_id text, p_new_input text, p_plugin_name text, p_plugin_func text, p_plugin_input text, p_plugin_output text, p_plugin_error text, p_promise_name text, p_promise_id text, p_promise_result text, p_promise_error text, p_payload jsonb, p_checksum text, p_tenant_id uuid) TO cleat_app;


