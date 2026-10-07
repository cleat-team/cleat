#!/usr/bin/env bash
#
# Create (or update) the cleat_app login, map it to a user, and add it to
# cleat_app_role -- cleat#2203.
#
# migrations/mssql/008_app_login.sql creates cleat_app_role (a database ROLE
# carrying the GRANTs and the DENY on deployment_secrets) and stops there,
# deliberately: CREATE LOGIN is a SERVER-level operation, needing securityadmin
# or sysadmin, and nothing else a SQL Server migration does needs more than
# db_owner. Measured against a db_owner-scoped login with no server role --
# which is everything develop asks of a SQL Server migrate login today, and
# which migrates develop cleanly -- running CREATE LOGIN as part of that
# migration failed with "User does not have permission to perform this
# action. (15247)" (cleat-review's G2). So the LOGIN is a deploy-time step,
# run once by an operator whose connection DOES have securityadmin or
# sysadmin -- the same division PostgreSQL already has between its migration
# (creates the role) and deploy/postgres/900-app-role.sh (gives it a login
# password), just split one principal earlier here because SQL Server's role
# and login are two separate object types where PostgreSQL's are one.
#
# The password is OPERATOR-SUPPLIED, via CLEAT_APP_PASSWORD -- the same
# convention deploy/mysql/900-app-role.sh already uses. An earlier draft had
# this script generate a random password itself, server-side, via NEWID(),
# and never print or return it: that closed cleat-review's G3 (no literal
# published) but opened a worse gap the same review caught on its next pass
# (N1) -- a login nobody, including the operator running this script, can
# authenticate as, since the only copy of the password was discarded the
# moment the script exited. Worse, re-running the script to pick up a new
# plugin's grants would silently reset cleat_app's password to a SECOND
# unknown value, locking out any worker already configured with the first.
# A random password is right for a login that is created disabled and never
# enabled without also setting a known one in the same step (G3's actual
# context, inside a migration) -- it is wrong for a step whose entire job is
# to hand the operator a working, usable login (this one).
#
# Required usage:
#   CLEAT_APP_PASSWORD=<password> \
#     ./deploy/mssql/900-app-role.sh <server> <admin-user> <admin-password> <database>
#
# <database> is the one this worker serves from -- cleat_app_role's GRANTs are
# schema-scoped (SCHEMA::dbo, SCHEMA::admin) within it. Run it again, with a
# different <database>, for a second application database. Re-run this script
# after enabling a plugin that was not installed this time.

set -euo pipefail

: "${CLEAT_APP_PASSWORD:?CLEAT_APP_PASSWORD must be set -- the password cleat_app will authenticate with}"

server="${1:?usage: $0 <server> <admin-user> <admin-password> <database>}"
admin_user="${2:?usage: $0 <server> <admin-user> <admin-password> <database>}"
admin_password="${3:?usage: $0 <server> <admin-user> <admin-password> <database>}"
database="${4:?usage: $0 <server> <admin-user> <admin-password> <database>}"

# The -v substitution below is TEXTUAL, not an escaped bind parameter: sqlcmd
# splices CLEAT_APP_PASSWORD's value directly into `N'$(CLEAT_APP_PASSWORD)'`
# with no quoting of its own. A password containing a single quote would
# break out of that literal and turn the rest of itself into T-SQL, the same
# class of defect as string-built SQL anywhere else in this tree. Reject it
# rather than doubling it here, because QUOTENAME below is the one place in
# this script already trusted to quote a value correctly, and giving it an
# ALREADY-escaped input would double-escape it.
case "$CLEAT_APP_PASSWORD" in
*\'*)
	echo "ERROR: CLEAT_APP_PASSWORD must not contain a single quote -- it is substituted" >&2
	echo "textually into a T-SQL string literal. Choose a password without one." >&2
	exit 1
	;;
esac

# -b is load bearing, and until it was added this script reported success on
# failure. sqlcmd returns 0 for a SQL error unless -b is given, so with the
# database's cleat_app_role missing (a migrate step that has not run, or ran a
# chain without this migration) the ALTER ROLE below failed, printed
# `Msg 15151 ... Cannot alter the role 'cleat_app_role', because it does not
# exist or you do not have permission.`, and the script then printed its own
# "cleat_app is ready ... a member of cleat_app_role" line and exited 0. The
# deployment gets a cleat_app that is not a member of the role, so every GRANT
# and the DENY on deployment_secrets are simply absent, and nothing failed.
# PostgreSQL never had this (its script passes -v ON_ERROR_STOP=1) and the mysql
# client exits non-zero on its own; SQL Server is the one dialect whose client
# has to be asked. Measured both ways: with -b the failing case above exits 1 and
# stops before the success line, and a genuinely successful run still exits 0.
sqlcmd_exec() {
	/opt/mssql-tools18/bin/sqlcmd -b -S "$server" -U "$admin_user" -P "$admin_password" -C -d "$database" "$@"
}

sqlcmd_exec -v CLEAT_APP_PASSWORD="${CLEAT_APP_PASSWORD}" <<-'SQL'
	DECLARE @pw NVARCHAR(128) = N'$(CLEAT_APP_PASSWORD)';

	IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'cleat_app' AND type = 'S')
	BEGIN
	    DECLARE @create NVARCHAR(MAX) = N'CREATE LOGIN cleat_app WITH PASSWORD = ' + QUOTENAME(@pw, N'''') + N';';
	    EXEC sp_executesql @create;
	END
	ELSE
	BEGIN
	    DECLARE @alter NVARCHAR(MAX) = N'ALTER LOGIN cleat_app WITH PASSWORD = ' + QUOTENAME(@pw, N'''') + N';';
	    EXEC sp_executesql @alter;
	END

	ALTER LOGIN cleat_app ENABLE;

	IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_app' AND type = 'S')
	    CREATE USER cleat_app FOR LOGIN cleat_app;

	IF NOT EXISTS (
	    SELECT 1 FROM sys.database_role_members rm
	    JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
	    JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
	    WHERE r.name = N'cleat_app_role' AND m.name = N'cleat_app'
	)
	    ALTER ROLE cleat_app_role ADD MEMBER cleat_app;
SQL

# Prove cleat_app came out without anything it should not have -- the same
# role this check plays in deploy/postgres/900-app-role.sh. sysadmin bypasses
# every DENY cleat_app_role carries, including the one on deployment_secrets.
sysadmin="$(sqlcmd_exec -h -1 -Q "SET NOCOUNT ON; SELECT CASE WHEN IS_SRVROLEMEMBER('sysadmin', 'cleat_app') = 1 THEN 'Y' ELSE 'N' END;" | tr -d '[:space:]')"
if [ "$sysadmin" = "Y" ]; then
	echo "ERROR: cleat_app is a sysadmin -- this exempts it from every DENY cleat_app_role carries." >&2
	exit 1
fi

echo "cleat_app is ready on ${database}: enabled, a member of cleat_app_role, not sysadmin."
