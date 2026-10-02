#!/usr/bin/env bash
#
# Run examples/b2b-saas-control-plane-dbos-port end to end against a real DBOS
# runtime and a real PostgreSQL, over HTTP -- the DBOS-side twin of
# scripts/run-b2b-saas-control-plane-scenario.sh. cleat#2597.
#
# WHY THIS EXISTS
#
# The port's own README states the claim this pair carries: an equally-scoped,
# idiomatic, EXECUTED DBOS counterpart exists, and building it shows that the
# tenant filter, the audit append and its replay idempotency are the port
# author's code rather than the platform's. An unexecuted port is not evidence
# for any of that. `npm test` (src/provision.test.ts) already exercises the
# workflow and the audit trail directly; this script exercises the OTHER half
# -- the HTTP backend (src/server.ts) -- the same way the cleat-side scenario
# drives cleat's backend rather than only unit-testing the workflow package.
#
# WHAT IT ASSERTS, driven through the HTTP surface:
#
#   1. a signup provisions a tenant to "active", and that tenant's own token
#      can read its own lifecycle row;
#   2. a signup whose workspace provisioning fails reports "failed" and names
#      the step;
#   3. one tenant's token never resolves another tenant's row;
#   4. a backdated trial is suspended by the REAL scheduled sweep -- this
#      script waits for the cron rather than firing the schedule itself, the
#      same way the cleat-side scenario waits for the real background loop.
#
# NOT A DIALECT MATRIX. cleat's own scenario runs three arms because this
# project's defect history is concentrated in SQL dialect divergence. The DBOS
# SDK's system database is Postgres-only; there is no second dialect on this
# side to diverge on, so one arm is the whole claim rather than a narrowed one.
#
# EXIT STATUS
#
#   0  every scenario asserted correctly
#   1  a scenario reported the wrong result -- a finding about the port
#   2  UNMEASURED -- a precondition (node, npm, the built server, a reachable
#      database) was not met. Never printed as though it were scenario 0.
set -uo pipefail

# ERREXIT OFF, EXPLICITLY, for the reason the sibling runner documents: several
# checks below read a non-zero exit (a curl timeout, a poll that never settles)
# as DATA, not as a reason to abort before the failure counter can record it.
set +e

cd "$(git rev-parse --show-toplevel)" || exit 2

EXAMPLE_DIR="examples/b2b-saas-control-plane-dbos-port"

if ! command -v node >/dev/null 2>&1; then
  echo "UNMEASURED: node is not available, so nothing here can be run." >&2
  exit 2
fi
if [[ ! -f "$EXAMPLE_DIR/package.json" ]]; then
  echo "UNMEASURED: $EXAMPLE_DIR/package.json is missing." >&2
  exit 2
fi

DB_URL="${DBOS_SYSTEM_DATABASE_URL:-}"
if [[ -z "$DB_URL" ]]; then
  echo "UNMEASURED: DBOS_SYSTEM_DATABASE_URL is not set." >&2
  echo "This script needs a reachable Postgres; it does not start one itself." >&2
  exit 2
fi

free_port() {
  python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
}

API_PORT="$(free_port)"
API="http://127.0.0.1:$API_PORT"
SERVER_LOG="$(mktemp -t b2b-dbos-server-XXXXXX.log)"
SERVER_PID=""

cleanup() {
  local rc=$?
  if (( rc != 0 )) && [[ -n "$SERVER_PID" ]]; then
    echo >&2
    echo "--- server (tail) ---" >&2
    tail -40 "$SERVER_LOG" >&2 2>&1 || true
  fi
  [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" >/dev/null 2>&1
  rm -f "$SERVER_LOG"
}
trap cleanup EXIT

cd "$EXAMPLE_DIR" || exit 2

echo "==> npm install"
if ! npm install --no-audit --no-fund >/tmp/b2b-dbos-npm-install.log 2>&1; then
  echo "FAIL: npm install" >&2
  tail -30 /tmp/b2b-dbos-npm-install.log >&2
  exit 1
fi

echo "==> npm run build"
if ! npm run build >/tmp/b2b-dbos-npm-build.log 2>&1; then
  echo "FAIL: npm run build" >&2
  tail -30 /tmp/b2b-dbos-npm-build.log >&2
  exit 1
fi

echo "==> starting the server on port $API_PORT"
DBOS_SYSTEM_DATABASE_URL="$DB_URL" PORT="$API_PORT" node dist/server.js >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

deadline=$((SECONDS + 60))
up=0
while (( SECONDS < deadline )); do
  if ! kill -0 "$SERVER_PID" >/dev/null 2>&1; then
    echo "FAIL: the server process exited before it ever answered" >&2
    tail -40 "$SERVER_LOG" >&2
    exit 1
  fi
  if curl -fsS --max-time 3 "$API/healthz" >/dev/null 2>&1; then
    up=1
    break
  fi
  sleep 1
done
if [[ "$up" -ne 1 ]]; then
  echo "FAIL: the server never answered GET /healthz within 60s" >&2
  tail -40 "$SERVER_LOG" >&2
  exit 1
fi

failures=0

# signup creates a tenant and starts its provisioning run. Echoes the raw JSON.
signup() {
  # $1: business name  $2..: extra JSON fields, each "key:jsonvalue"
  local name="$1"; shift
  local extra=""
  for kv in "$@"; do
    extra="${extra}, \"${kv%%:*}\": ${kv#*:}"
  done
  curl -fsS --max-time 15 -X POST "$API/api/signup" \
    -H 'Content-Type: application/json' \
    -d "{\"businessName\":\"$name\",\"adminEmail\":\"admin@example.com\",\"plan\":\"starter\"${extra}}"
}

json_field() {
  python3 -c "import json,sys; print(json.load(sys.stdin).get('$1',''))"
}

# poll_provisioning waits for a run to leave its lifecycle states and reports
# the business status, the same distinction the server's own route draws.
poll_provisioning() {
  local wf="$1"
  local deadline=$((SECONDS + 60))
  local status=""
  while (( SECONDS < deadline )); do
    status="$(curl -fsS --max-time 5 "$API/api/provisioning/$wf" 2>/dev/null | json_field status 2>/dev/null)"
    if [[ -n "$status" && "$status" != "PENDING" && "$status" != "ENQUEUED" ]]; then
      echo "$status"
      return 0
    fi
    sleep 1
  done
  echo "$status"
}

# lifecycle reads the caller's own tenant row. $2 is the Bearer token.
lifecycle() {
  curl -sS --max-time 5 "$API/api/tenant/lifecycle" -H "Authorization: Bearer $1"
}

assert_eq() {
  local scenario="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then
    echo "ok: $scenario -> $got"
  else
    echo "FAIL: $scenario -- got '$got', want '$want'" >&2
    failures=$((failures + 1))
  fi
}

assert_ne() {
  local scenario="$1" got="$2" unwanted="$3"
  if [[ "$got" != "$unwanted" ]]; then
    echo "ok: $scenario"
  else
    echo "FAIL: $scenario -- got '$got', which is the value it must never be" >&2
    failures=$((failures + 1))
  fi
}

echo
echo "==> scenario: a clean signup provisions to active"
resp="$(signup 'Acme Widgets')"
wf="$(echo "$resp" | json_field workflowID)"
tenant_a="$(echo "$resp" | json_field tenantId)"
key_a="$(echo "$resp" | json_field apiKey)"
assert_eq "clean provisioning" "$(poll_provisioning "$wf")" "active"
assert_eq "the tenant's own token reads its own row" \
  "$(lifecycle "$key_a" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tenant"]["tenant_id"])')" \
  "$tenant_a"

echo
echo "==> scenario: a workspace failure fails the run and names the step"
resp="$(signup 'Broken Workspaces' 'simulateWorkspaceFailure:true')"
wf="$(echo "$resp" | json_field workflowID)"
assert_eq "workspace failure" "$(poll_provisioning "$wf")" "failed"
assert_eq "the failing step is published" \
  "$(curl -fsS --max-time 5 "$API/api/provisioning/$wf" | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["failedStep"])')" \
  "provision_workspace"

echo
echo "==> scenario: one tenant's token never resolves another's row"
resp_b="$(signup 'Other Business')"
tenant_b="$(echo "$resp_b" | json_field tenantId)"
key_b="$(echo "$resp_b" | json_field apiKey)"
assert_eq "tenant B's token reads tenant B" \
  "$(lifecycle "$key_b" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tenant"]["tenant_id"])')" \
  "$tenant_b"
assert_ne "tenant A's token is not tenant B" "$(lifecycle "$key_a" | json_field tenant_id)" "$tenant_b"
# An unauthenticated caller gets nothing, which is the boundary this port has
# INSTEAD of the row-level security the cleat side has underneath.
assert_eq "no token is refused" \
  "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$API/api/tenant/lifecycle")" "401"

echo
echo "==> scenario: the SCHEDULED sweep suspends a backdated trial"
# Backdated the way the cleat-side scenario does it. Done through the example's
# own pg client rather than psql, because psql is not guaranteed on the runner
# and node is (this script already requires it).
# SC2016 is suppressed deliberately, not overlooked: the `$1` in the
# single-quoted JS below is a pg PLACEHOLDER, not a shell variable, and
# expanding it before node saw it would be the bug. Verified against the
# pinned shellcheck (0.11.0) rather than silenced by reflex -- without this
# the script exits 1 and the required Lint job fails, and the sibling runners
# pass clean, which is what distinguishes a real finding from linter noise.
# shellcheck disable=SC2016
if ! node -e '
const { Pool } = require("pg");
const pool = new Pool({ connectionString: process.env.DBOS_SYSTEM_DATABASE_URL });
pool.query("UPDATE b2b_tenants SET trial_expires_at = now() - interval \x271 day\x27 WHERE tenant_id = $1", [process.argv[1]])
  .then(() => pool.end())
  .catch((e) => { console.error(e); process.exit(1); });
' "$tenant_b"; then
  echo "FAIL: could not backdate the trial" >&2
  failures=$((failures + 1))
fi

# WAITING FOR THE REAL SCHEDULE, not firing it: the sweep is registered as a
# per-minute cron, so this is bounded. Jumping it with triggerSchedule is what
# the unit test does; doing it here would mean this script never exercised the
# registration at all.
swept=""
deadline=$((SECONDS + 150))
while (( SECONDS < deadline )); do
  if [[ "$(lifecycle "$key_b" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tenant"]["suspended"])' 2>/dev/null)" == "True" ]]; then
    swept="suspended"
    break
  fi
  sleep 5
done
assert_eq "the scheduled sweep suspends an expired trial" "$swept" "suspended"
assert_eq "a tenant inside its trial is left alone" \
  "$(lifecycle "$key_a" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tenant"]["suspended"])')" \
  "False"

echo
echo "==> scenario: the API is rate-limited"
# The FOURTH thing this port writes that cleat's platform supplies. cleat's
# worker rate-limits as a platform feature (plugins/ratelimiter, plus an
# ipRateLimiter and a keyedRateLimiter in cmd/cleat-worker/main.go), so an
# example proxied through it is bounded without the example doing anything;
# here the limiter is this port's own code. Asserted rather than assumed,
# because "the limiter is configured" and "the limiter refuses" are different
# claims and only the second one is the point.
limited=0
for _ in $(seq 1 140); do
  code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$API/api/tenant/lifecycle")"
  if [[ "$code" == "429" ]]; then
    limited=1
    break
  fi
done
assert_eq "a route past its allowance is refused with 429" "$limited" "1"

echo
if (( failures > 0 )); then
  echo "$failures assertion(s) failed" >&2
  exit 1
fi
echo "all scenarios passed"
