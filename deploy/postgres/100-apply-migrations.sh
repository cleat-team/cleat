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
	psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" -f "$f"
done

echo "cleat: applied ${#files[@]} migration file(s)."
