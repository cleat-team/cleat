#!/usr/bin/env bash
#
# Give the cleat_app role a password so the workers can connect as it.
#
# migrations/postgres/005_app_role.sql creates cleat_app NOLOGIN and grants it
# what the engine needs. It deliberately stops there: a password does not
# belong in a file that is committed to the repository, mounted into
# containers, and re-applied by every worker at boot. Supplying it is the
# deployment's job, and this script is that job for
# docker-compose.cluster.yml.
#
# Why a separate role at all: every tenant-scoped table has row-level security
# enabled and FORCEd, and for GetWorkflowByID and ListWorkflows those policies
# are the only tenant isolation there is. PostgreSQL never applies RLS to a
# superuser, and POSTGRES_USER (here, "cleat") is one -- so before this, the
# policies were bypassed by every connection that ever ran against them.
#
# Runs from docker-entrypoint-initdb.d after the numbered migrations, which is
# what the 900 prefix is for. Note the entrypoint only runs initdb.d on an
# *empty* data directory: on an existing deployment, run the ALTER ROLE below
# by hand.

set -euo pipefail

: "${POSTGRES_USER:?POSTGRES_USER must be set}"
: "${POSTGRES_DB:?POSTGRES_DB must be set}"

if [ -z "${CLEAT_APP_PASSWORD:-}" ]; then
	# Failing closed. Without a password the role cannot log in, the workers
	# cannot connect, and the operator gets this message instead of a cluster
	# that silently falls back to a superuser connection.
	echo "ERROR: CLEAT_APP_PASSWORD is not set." >&2
	echo "The cleat_app role has no password, so no worker can connect as it." >&2
	echo "Set CLEAT_APP_PASSWORD in the environment of the postgres service." >&2
	exit 1
fi

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-SQL
	ALTER ROLE cleat_app LOGIN PASSWORD '${CLEAT_APP_PASSWORD}';
SQL

# Prove the role came out the way it is supposed to, rather than assuming the
# migration ran. A cleat_app that is somehow a superuser, or has BYPASSRLS,
# would pass every functional test and isolate nothing.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-'SQL'
	DO $$
	DECLARE r record;
	BEGIN
	    SELECT rolsuper, rolbypassrls, rolcanlogin INTO r
	    FROM pg_roles WHERE rolname = 'cleat_app';
	    IF NOT FOUND THEN
	        RAISE EXCEPTION 'cleat_app does not exist: 005_app_role.sql did not run';
	    END IF;
	    IF r.rolsuper OR r.rolbypassrls THEN
	        RAISE EXCEPTION 'cleat_app is exempt from row-level security (superuser=% bypassrls=%)',
	            r.rolsuper, r.rolbypassrls;
	    END IF;
	    IF NOT r.rolcanlogin THEN
	        RAISE EXCEPTION 'cleat_app still cannot log in after ALTER ROLE';
	    END IF;
	END $$;
SQL

echo "cleat_app is ready: login enabled, not a superuser, no BYPASSRLS."
