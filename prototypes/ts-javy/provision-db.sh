#!/usr/bin/env bash
# Give a run of this harness its OWN database, so it can run beside another run (a reviewer re-deriving a checkpoint, say) without the
# two sets of workers claiming each other's workflow runs. Idempotent: an already-provisioned database with a key file is left alone.
#   CLEAT_PROTO_PG_DB=cleat_p3 CLEAT_PROTO_API_PORT=18772 CLEAT_PROTO_KEY_FILE=.work/p3.key ./provision-db.sh
# Then run everything with the same three variables exported.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
read -r DB CONT KEYF OWN APPD < <(python3 - <<'PY'
import phase1_lib as L
print(L.PG_DB, L.PG, L.KEY_FILE, L.OWNER, L.APP)
PY
)
if docker exec "$CONT" psql -U postgres -At -c "select 1 from pg_database where datname='$DB'" | grep -q 1; then
  echo "database $DB exists"
else
  docker exec "$CONT" psql -U postgres -At -c "CREATE DATABASE \"$DB\"" >/dev/null && echo "created database $DB"
fi
.work/cleat-worker -db "$APPD" -migrate-db "$OWN" -migrate-only 2>&1 | tail -1 | cut -c1-120
if [ -s "$KEYF" ]; then echo "key file $KEYF exists, kept"; exit 0; fi
key=$(.work/cleat-worker -db "$OWN" -generate-api-key 00000000-0000-0000-0000-000000000000 2>&1 | grep -oE 'cleat_sk_[0-9a-f]+' | head -1)
[ -n "$key" ] || { echo "UNMEASURED: no API key was generated for $DB" >&2; exit 2; }
umask 077; printf '%s\n' "$key" > "$KEYF"; echo "wrote $KEYF"
