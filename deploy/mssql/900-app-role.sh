#!/usr/bin/env bash
#
# Create (or update) the cleat_app login, map it to a user, and add it to
# cleat_app_role -- cleat#2203.
#
# migrations/mssql/007_app_login.sql creates cleat_app_role (a database ROLE
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
# The password is a SERVER-GENERATED random value, not a literal in this
# file: an earlier draft used a fixed throwaway string, created-then-disabled,
# on the reasoning that a disabled login cannot authenticate with any
# password. That is true until the FIRST enable -- `ALTER LOGIN cleat_app
# ENABLE` alone, with no password change, would have published that literal
# as cleat_app's real password (cleat-review's G3). This script sets a real,
# unknowable password and enables the login in the same run, so there is no
# window where "enabled" and "a published credential" coincide.
#
# Required usage:
#   ./deploy/mssql/900-app-role.sh <server> <admin-user> <admin-password> <database>
#
# <database> is the one this worker serves from -- cleat_app_role's GRANTs are
# schema-scoped (SCHEMA::dbo, SCHEMA::admin) within it. Run it again, with a
# different <database>, for a second application database.

set -euo pipefail

server="${1:?usage: $0 <server> <admin-user> <admin-password> <database>}"
admin_user="${2:?usage: $0 <server> <admin-user> <admin-password> <database>}"
admin_password="${3:?usage: $0 <server> <admin-user> <admin-password> <database>}"
database="${4:?usage: $0 <server> <admin-user> <admin-password> <database>}"

sqlcmd_exec() {
	/opt/mssql-tools18/bin/sqlcmd -S "$server" -U "$admin_user" -P "$admin_password" -C -d "$database" "$@"
}

# The password lives only in this SQLCMDPASSWORD-style script variable, passed
# with -v rather than interpolated into the heredoc: a $(VAR) token left
# UNDEFINED is not a failure sqlcmd reports on its own -- it is substituted as
# the LITERAL TEXT "$(CLEAT_APP_PASSWORD)" and the script carries on, exit 0,
# which would make `WITH PASSWORD = '$(CLEAT_APP_PASSWORD)'` a real, fixed
# password nobody chose. Measured directly. Passing it as a shell-constructed
# -v argument means an unset or empty value is a bash parameter failure
# (":?") before sqlcmd ever runs, not a silent literal.
random_password="$(/opt/mssql-tools18/bin/sqlcmd -S "$server" -U "$admin_user" -P "$admin_password" -C -h -1 -Q "SET NOCOUNT ON; SELECT CONVERT(NVARCHAR(36), NEWID()) + CONVERT(NVARCHAR(36), NEWID()) + N'Aa1!';" | tr -d '[:space:]')"
: "${random_password:?failed to generate a random password from the server}"

sqlcmd_exec -v CLEAT_APP_PASSWORD="${random_password}" <<-'SQL'
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
