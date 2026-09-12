#!/usr/bin/env bash
#
# Apply migrations/postgres/*.sql to a freshly initialised database.
#
# The compose file used to mount migrations/postgres directly as
# /docker-entrypoint-initdb.d and rely on the entrypoint running *.sql in
# lexical order. That worked, but it made the directory serve two masters: it
# is the migration set the Go runner reads, and it was also the literal
# contents of initdb.d, so nothing else could be put there. Mounting a second
# file over the top is not possible either -- a bind mount inside a read-only
# bind mount fails at container init with
#
#   create mountpoint for /docker-entrypoint-initdb.d/900-app-role.sh:
#   read-only file system
#
# So the migrations are mounted read-only somewhere neutral and applied from
# here. The ordering is now stated rather than inferred from a directory
# listing.

set -euo pipefail

: "${POSTGRES_USER:?POSTGRES_USER must be set}"
: "${POSTGRES_DB:?POSTGRES_DB must be set}"

MIGRATIONS_DIR="${CLEAT_MIGRATIONS_DIR:-/opt/cleat/migrations}"

# The schema cleat builds into, matching cmd/cleat-worker's --schema.
#
# Until cleat#1287 this was not a variable at all: nineteen of the files opened
# with `SET search_path = public;` and carried the answer themselves. They no
# longer do -- migration.Runner sets it on its own connection instead, because
# a value stated in nineteen places and omitted in twenty-five cannot follow a
# flag. This path has no runner, so it has to supply the same two things the
# runner does: the schema must exist, and search_path must point at it.
#
# PGOPTIONS rather than a -c before each -f, because each psql invocation below
# is its own session and the setting has to be there from the first statement.
CLEAT_SCHEMA="${CLEAT_SCHEMA:-public}"

case "$CLEAT_SCHEMA" in
	[A-Za-z_]*) : ;;
	*) echo "ERROR: CLEAT_SCHEMA=$CLEAT_SCHEMA is not a plain identifier." >&2; exit 1 ;;
esac
case "$CLEAT_SCHEMA" in
	*[!A-Za-z0-9_]*) echo "ERROR: CLEAT_SCHEMA=$CLEAT_SCHEMA is not a plain identifier." >&2; exit 1 ;;
esac

if [ "$CLEAT_SCHEMA" != "public" ]; then
	psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
		-c "CREATE SCHEMA IF NOT EXISTS \"$CLEAT_SCHEMA\""
fi
# pg_temp last, because the SECURITY DEFINER functions created with
# `SET search_path FROM CURRENT` freeze this value onto themselves.
# (No count: this said "four" and there are three. Five migration files carry
# the statement, and 040's admin.claim_workflows is superseded by 055's.)
# PostgreSQL searches pg_temp first when it is not named, which inside a
# function holding an RLS exemption is a shadowing hazard.
export PGOPTIONS="--search_path=$CLEAT_SCHEMA,pg_temp"

if [ ! -d "$MIGRATIONS_DIR" ]; then
	echo "ERROR: $MIGRATIONS_DIR is not mounted." >&2
	echo "The compose file must mount ./migrations/postgres there." >&2
	exit 1
fi

# A run that applies nothing is far more likely to be a broken mount than an
# empty migration set, and it would leave a database that looks initialised and
# has no schema -- which is exactly the failure mode that took a full session
# to find last time.
shopt -s nullglob
files=("$MIGRATIONS_DIR"/*.sql)
if [ ${#files[@]} -eq 0 ]; then
	echo "ERROR: no .sql files in $MIGRATIONS_DIR." >&2
	exit 1
fi

# Sorted explicitly: the numeric prefixes exist to encode order, and relying on
# the shell's glob collation to honour them is a dependency on the locale.
readarray -t files < <(printf '%s\n' "${files[@]}" | LC_ALL=C sort)

for f in "${files[@]}"; do
	echo "cleat: applying $(basename "$f")"
	# PGOPTIONS above is the whole of what this path has to supply. The
	# statements that cannot be written relative to search_path -- GRANT ...
	# ON SCHEMA, ALTER ROLE -- ask for it with current_schema() inside a DO
	# block, and the SECURITY DEFINER attributes use FROM CURRENT. Nothing
	# here substitutes anything, which is deliberate: an earlier version of
	# this fix used a psql variable, and the repository has nine other places
	# that apply these files with a raw Exec and would each have had to honour
	# the same contract.
	psql -v ON_ERROR_STOP=1 \
		--username "$POSTGRES_USER" --dbname "$POSTGRES_DB" -f "$f"
done

echo "cleat: applied ${#files[@]} migration file(s)."
