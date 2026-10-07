#!/usr/bin/env bash
#
# Create (or update) the cleat_app login, give it a password, and grant it
# everything a worker serving as it needs -- cleat#2203.
#
# This does NOT live in migrations/mysql/ the way PostgreSQL's equivalent
# (cleat_app in migrations/postgres/001_schema.sql) does. CREATE USER needs
# the global CREATE USER privilege, and GRANT needs GRANT OPTION on every
# privilege being granted -- neither is something a MySQL migrate login has
# ever needed. Measured against a login scoped to `ALL ON <db>.*` -- which is
# everything develop asks of a MySQL migrate login today, and which migrates
# develop cleanly -- running this as a migration failed with
# "Error 1227 (42000): Access denied; you need (at least one of) the
# CREATE USER privilege(s)". Shipping it as a migration would have broken
# every upgrade whose migrate login is scoped the way MySQL deployments are
# documented to scope it (cleat-review's G2). So this is a deploy-time step,
# run once by an operator whose connection DOES have CREATE USER and GRANT
# OPTION -- `root`, or an account built for exactly this.
#
# The password is set in the SAME statement that creates the account, unlike
# PostgreSQL's NOLOGIN-then-ALTER-ROLE-LOGIN two-step (and this script's own
# earlier draft, which created the account ACCOUNT LOCK with no password at
# all): CREATE USER ... IDENTIFIED BY '<empty>' is an empty password the
# instant the account is unlocked, and there is no migration-tracked baseline
# step here to make a deliberately-dormant intermediate state meaningful --
# every run of this script already needs the real password supplied, so there
# is no reason to create the account without it even briefly (cleat-review's
# G3). Re-run this script to rotate the password: ALTER USER below always sets
# it to match CLEAT_APP_PASSWORD, whether the account is new or already exists.
#
# THE TABLE LIST IS QUERIED, NOT HARDCODED -- and this is required, not a
# style choice. An earlier draft listed the 29 core tables by name, the same
# way migrations/mysql/001_schema.sql creates them. Running a real worker as
# the result, with a plugin enabled, failed immediately: `rate_limits`,
# `oauth_sessions`, `tenant_trials`, `task_queue`, `schedules`,
# `backup_config` and `backup_history` are created by PLUGIN migrations
# (plugin.RunMigrations), which run after the core chain and whose table set
# depends on which plugins a deployment enables. PostgreSQL covers this with
# `ALTER DEFAULT PRIVILEGES`; SQL Server's 008_app_login.sql covers it with a
# schema-level GRANT; MySQL has neither mechanism, and a hardcoded list is
# exactly the growing-population census CLAUDE.md warns never stays correct.
# Querying information_schema.tables at RUN time is the only form of this
# that does not need updating every time a plugin adds a table -- the cost is
# that enabling a NEW plugin after this script has run means running it
# again, which the final message below says explicitly.
#
# Required usage:
#   CLEAT_APP_PASSWORD=<password> \
#     ./deploy/mysql/900-app-role.sh <host> <port> <admin-user> <admin-password> <database>
#
# <database> is the ONE application database this worker serves from. Run it
# again for a second application database if this deployment has more than
# one, and again whenever a newly-enabled plugin adds tables this script has
# not yet seen.

set -euo pipefail

: "${CLEAT_APP_PASSWORD:?CLEAT_APP_PASSWORD must be set -- the password cleat_app will authenticate with}"

# CLEAT_APP_PASSWORD is spliced directly into `IDENTIFIED BY '${CLEAT_APP_PASSWORD}'`
# below with no escaping -- a password containing a single quote would break out of
# that literal and turn the rest of itself into SQL, the same class of defect N1 found
# in this script's SQL Server sibling. Reject it here for the same reason: this script
# has no QUOTENAME-equivalent anywhere to hand the value to instead.
#
# A backslash is a second, quieter way to corrupt the same literal: MySQL treats
# backslash as a string-literal escape character by default (NO_BACKSLASH_ESCAPES is
# not assumed here), so a password containing one is not rejected by the connection --
# it is silently REWRITTEN (`\n` becomes a newline, `\\` collapses to one backslash),
# and the password actually set no longer matches what the operator typed. Reject it
# rather than doubling it, same reasoning as the quote above (cleat-review's A3).
case "$CLEAT_APP_PASSWORD" in
*\'*)
	echo "ERROR: CLEAT_APP_PASSWORD must not contain a single quote -- it is substituted" >&2
	echo "textually into a SQL string literal. Choose a password without one." >&2
	exit 1
	;;
*\\*)
	echo "ERROR: CLEAT_APP_PASSWORD must not contain a backslash -- MySQL treats it as a" >&2
	echo "string-literal escape character, so it would silently change the password" >&2
	echo "actually set. Choose a password without one." >&2
	exit 1
	;;
esac

host="${1:?usage: $0 <host> <port> <admin-user> <admin-password> <database>}"
port="${2:?usage: $0 <host> <port> <admin-user> <admin-password> <database>}"
admin_user="${3:?usage: $0 <host> <port> <admin-user> <admin-password> <database>}"
admin_password="${4:?usage: $0 <host> <port> <admin-user> <admin-password> <database>}"
database="${5:?usage: $0 <host> <port> <admin-user> <admin-password> <database>}"

# Fail closed on a main database whose name is ALSO a tenant database, by the
# pattern cleat_app's own grant below uses. `cleat\_%` is a wildcard MySQL
# GRANT, not a prefix cleat_app is denied outside of -- a privilege granted
# at that pattern applies to EVERY matching database, including this one if
# its name happens to match. Measured directly, in this script's own test
# suite: `cmd/cleat-worker`'s scratch databases are named
# `cleat_2117_deploy_<n>` (a convention this script knows nothing about and
# cannot special-case), which matches `cleat\_%`, and a cleat_app granted
# both the per-table write restriction above AND the wildcard below could
# write deployment_secrets in a database matching both -- the database-level
# wildcard grant is unconditional and MySQL has no DENY to narrow it back
# down, the same reason the per-table grants above are issued individually
# in the first place. A deployment naming its main database "cleat" (every
# example in this repo's docs) never collides; one naming it "cleat_prod" or
# similar would, silently, without this check.
case "$database" in
cleat_*)
	echo "ERROR: ${database} matches the tenant-database pattern cleat_app is granted" >&2
	echo "ALL PRIVILEGES on (cleat\\_%) below -- granting it here would also grant write" >&2
	echo "on this database's own deployment_secrets. Rename the main application" >&2
	echo "database so it does not start with 'cleat_', or use a different value for" >&2
	echo "<database> than the literal tenant-database prefix." >&2
	exit 1
	;;
esac

mysql_exec() {
	MYSQL_PWD="$admin_password" mysql --host="$host" --port="$port" --user="$admin_user" \
		--database="$database" --batch --skip-column-names "$@"
}

mysql_exec <<-SQL
	CREATE USER IF NOT EXISTS 'cleat_app'@'%' IDENTIFIED BY '${CLEAT_APP_PASSWORD}';
	ALTER USER 'cleat_app'@'%' IDENTIFIED BY '${CLEAT_APP_PASSWORD}';
SQL

# Every base table in THIS database gets SELECT, including deployment_secrets
# (read is fine -- see below for why write is not). information_schema is not
# itself in the result: it is a separate, server-wide pseudo-database, never
# the value of `--database`.
tables="$(mysql_exec -e "SELECT table_name FROM information_schema.tables WHERE table_schema = '${database}' AND table_type = 'BASE TABLE'")"
if [ -z "$tables" ]; then
	echo "ERROR: ${database} has no tables -- has the migrate step run yet?" >&2
	exit 1
fi

{
	while IFS= read -r t; do
		[ -n "$t" ] && echo "GRANT SELECT ON \`${t}\` TO 'cleat_app'@'%';"
	done <<<"$tables"

	# INSERT/UPDATE/DELETE go to every table EXCEPT deployment_secrets. Values
	# go in and are retired through cleatctl (set-deployment-secret,
	# retire-deployment-secret, reseal-deployment-secrets), run with the
	# owner/migrate login -- see docs/how-to/use-deployment-secrets.md. There
	# is deliberately no single GRANT covering every table here: MySQL
	# privilege checks are an OR across whatever grant tables apply (global,
	# database, table), with no DENY to override a broader one, so the only
	# way to keep deployment_secrets out is to never grant it write and grant
	# every other table individually, which is what this loop does.
	while IFS= read -r t; do
		[ -n "$t" ] && [ "$t" != "deployment_secrets" ] && echo "GRANT INSERT, UPDATE, DELETE ON \`${t}\` TO 'cleat_app'@'%';"
	done <<<"$tables"
} | mysql_exec

# Every stored procedure and function gets EXECUTE, queried the same way and
# for the same reason as the tables above -- finalize_workflow_status is
# CALLed on every workflow completion (engine/mysql_store.go), and without
# EXECUTE the worker cannot finish a single run as cleat_app. A plugin that
# ships its own routine needs this the same way a plugin's table needs a row
# in the loop above.
routines="$(mysql_exec -e "SELECT routine_name FROM information_schema.routines WHERE routine_schema = '${database}'")"
if [ -n "$routines" ]; then
	while IFS= read -r r; do
		[ -n "$r" ] && echo "GRANT EXECUTE ON PROCEDURE \`${r}\` TO 'cleat_app'@'%';"
	done <<<"$routines" | mysql_exec
fi

# MySQL has no row-level security and no schema below a database, so it
# isolates tenants with one database per tenant instead
# (engine.MySQLTenantDatabaseName: `cleat_<tenant-id-with-underscores>`),
# created lazily -- including for the default tenant, at the worker's first
# boot. That creation, and the full migration chain this database's own
# schema came from, replay against each tenant database through the SAME
# connection cleat_app serves on (engine/mysql_store.go's masterDB), so
# cleat_app needs CREATE and full DDL/DML there too, or a worker cannot boot
# at all on MySQL (cleat-review's G1, measured: "Access denied for user
# 'cleat_app'@'%' to database 'cleat_00000000_...'").
#
# This DOES give cleat_app write access to each tenant database's own copy of
# deployment_secrets -- every table above is replayed into every tenant
# database too. That copy is never read: engine.NewDeploymentSecretStore is
# constructed exactly once per process (cmd/cleat-worker/main.go), on the
# single top-level `db` connection this script's own per-table GRANTs apply
# to, and cleatctl's deployment-secret commands (set/retire/reseal) take
# their own `--db` argument directly -- nothing in the tree ever points a
# DeploymentSecretStore at a per-tenant connection. So the restriction this
# script exists to add holds everywhere it is actually exercised; the
# per-tenant copies are inert, unused tables that happen to exist because the
# full schema is replayed into every tenant database.
mysql_exec <<-'SQL'
	GRANT ALL PRIVILEGES ON `cleat\_%`.* TO 'cleat_app'@'%';
SQL

# Prove cleat_app came out without anything it should not have -- the same
# role this check plays in deploy/postgres/900-app-role.sh.
super="$(mysql_exec -e "SELECT super_priv FROM mysql.user WHERE user='cleat_app' AND host='%'")"
if [ "$super" = "Y" ]; then
	echo "ERROR: cleat_app has SUPER -- this script or something else granted it." >&2
	exit 1
fi

table_count="$(echo "$tables" | grep -c .)"
echo "cleat_app is ready on ${database}: password set, not SUPER, granted across ${table_count} table(s)."
echo "Re-run this script after enabling a plugin that was not installed this time."
