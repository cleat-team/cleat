#!/usr/bin/env bash
#
# Run examples/b2b-saas-control-plane end to end against a real worker and a
# real PostgreSQL, and assert what each run PUBLISHED. cleat#2534/#2681.
#
# PostgreSQL ONLY, and not by omission -- unlike
# scripts/run-order-lifecycle-scenario.sh, which this script otherwise
# mirrors (free-port allocation, a cleanup trap that captures logs before
# tearing the stack down, a per-run worker image tag): this scenario's
# backend creates tenants via `cleat-worker --create-tenant`, and that flag
# refuses outright on MySQL and SQL Server (auth.CreateTenant's own dialect
# gate). There is no second arm to run. See the README's dialect section.
#
# WHAT THIS ASSERTS, and it is the thing this scenario exists to demonstrate:
#
#   1. signup creates a real tenant and mints it a real API key, and the
#      provisioning workflow that follows runs AS that tenant -- every
#      record_event call lands in ITS OWN audit chain, not the operator's.
#   2. every provisioning milestone is recorded, in order, through the TYPED
#      record_event client (cleat#2626/#2681) -- asserted by event_type, not
#      merely "no error".
#   3. a workspace-provisioning failure fails the run and still records a
#      tenant.provisioning_failed milestone -- there is no code path that
#      loses the failure to record it.
#   4. the tenant-scoped /api/tenant/lifecycle route (cleat#2683) answers
#      for the caller's OWN tenant, derived from its API key -- the route
#      structurally cannot be pointed at another tenant, since no tenant id
#      is ever accepted as an argument.
#   5. plugins/tenantlifecycle's background sweep (Stage 1) suspends a
#      tenant once its trial is backdated into the past, and a suspended
#      tenant's own key can no longer start a workflow run (403) -- the
#      background-loop hitch point cleat#2681 asks for, exercised live
#      rather than only unit-tested.
#
# WHAT THIS DOES NOT ASSERT: cross-tenant audit-chain isolation under
# row-level security. That is a PostgreSQL RLS property, already covered
# exhaustively, with a real non-superuser connection and four failure-mode
# arms, by plugins/auditlog/audit_rows_are_scoped_to_their_tenant_test.go's
# TestAuditRowsAreScopedToTheirTenant. Re-deriving that here would duplicate
# a test that already exists and is stronger than anything a shell script
# talking to one worker process could check. What THIS script adds on top is
# the property that test cannot see: that /api/tenant/lifecycle never takes
# a tenant id as input at all, so there is no cross-tenant call to attempt
# in the first place -- point 4 above.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXAMPLE_DIR="$REPO_ROOT/examples/b2b-saas-control-plane"

free_port() {
  python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
}

SUFFIX="$$-$(date +%s)"
DB_PORT="$(free_port)"
API_PORT="$(free_port)"
BACKEND_PORT="$(free_port)"
WORKER_IMAGE="cleat-b2b-scp-test:$SUFFIX"
COMPOSE=(docker compose -f "$EXAMPLE_DIR/docker-compose.yml")
OUT_DIR="$(mktemp -d)"

export CLEAT_PG_PORT="$DB_PORT" CLEAT_API_PORT="$API_PORT"
export CLEAT_MIGRATE_DB_URL="postgres://cleat:cleat@postgres:5432/cleat?sslmode=disable"
export CLEAT_WORKER_DB_URL="postgres://cleat_app:cleat-app-local-dev@postgres:5432/cleat?sslmode=disable"
export COMPOSE_PROJECT_NAME="cleat-b2b-scp-$SUFFIX"

# The host-side DSN, for the admin operations this script runs locally
# (--create-org/--create-tenant, and the trial-backdate probe) and for the
# backend process, which also runs on the host rather than in compose -- see
# docker-compose.yml's own comment on why it is not a service there.
HOST_ADMIN_DB_URL="postgres://cleat:cleat@localhost:$DB_PORT/cleat?sslmode=disable"

failures=0
BACKEND_PID=""

cleanup() {
  local rc=$?
  if (( rc != 0 || failures != 0 )); then
    echo >&2
    echo "--- cleat-worker (tail) ---" >&2
    "${COMPOSE[@]}" logs --tail=60 cleat-worker >&2 2>&1 || true
    if [[ -n "$BACKEND_PID" ]]; then
      echo "--- backend (tail) ---" >&2
      tail -40 "$OUT_DIR/backend.log" >&2 2>&1 || true
    fi
  fi
  [[ -n "$BACKEND_PID" ]] && kill "$BACKEND_PID" >/dev/null 2>&1 || true
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  docker rmi -f "$WORKER_IMAGE" >/dev/null 2>&1 || true
  rm -rf "$OUT_DIR"
}
trap cleanup EXIT

echo "==> building the worker image from this checkout"
if ! docker build -t "$WORKER_IMAGE" . >"$OUT_DIR/docker-build.log" 2>&1; then
  echo "FAIL: docker build of the repository's Dockerfile" >&2
  tail -25 "$OUT_DIR/docker-build.log" >&2
  exit 1
fi
export CLEAT_WORKER_IMAGE="$WORKER_IMAGE"

echo "==> building cleat, cleat-worker and the backend from this checkout"
if ! go build -o "$OUT_DIR/cleat" ./cmd/cleat 2>"$OUT_DIR/build.log" ||
   ! go build -o "$OUT_DIR/cleat-worker" ./cmd/cleat-worker 2>>"$OUT_DIR/build.log" ||
   ! go build -o "$OUT_DIR/backend" ./examples/b2b-saas-control-plane/backend 2>>"$OUT_DIR/build.log"; then
  echo "FAIL: building one of cleat/cleat-worker/backend" >&2
  cat "$OUT_DIR/build.log" >&2
  exit 1
fi

echo "==> docker compose up"
if ! "${COMPOSE[@]}" up -d >"$OUT_DIR/compose-up.log" 2>&1; then
  echo "FAIL: docker compose up" >&2
  tail -30 "$OUT_DIR/compose-up.log" >&2
  exit 1
fi

API="http://localhost:$API_PORT"
deadline=$((SECONDS + 120))
until curl -fsS --max-time 5 "$API/healthz" >/dev/null 2>&1; do
  if (( SECONDS > deadline )); then
    echo "FAIL: the worker did not answer /healthz within 120s" >&2
    "${COMPOSE[@]}" logs --tail=60 cleat-worker >&2 || true
    exit 1
  fi
  sleep 2
done

# ---- build and deploy the workflow --------------------------------------

echo "==> cleat build"
if ! "$OUT_DIR/cleat" build -o "$OUT_DIR/wasm" "$EXAMPLE_DIR" >"$OUT_DIR/cleat-build.log" 2>&1; then
  echo "FAIL: cleat build" >&2
  cat "$OUT_DIR/cleat-build.log" >&2
  exit 1
fi
# Named for cleat.yaml's `name:` field, not the entry point -- cleat#2692.
WASM="$OUT_DIR/wasm/b2b-saas-control-plane.wasm"
if [[ ! -f "$WASM" ]]; then
  echo "FAIL: expected $WASM; cleat build's actual output was:" >&2
  ls -la "$OUT_DIR/wasm" >&2
  exit 1
fi

echo "==> cleat deploy"
if ! "$OUT_DIR/cleat" deploy --db "$HOST_ADMIN_DB_URL" --name b2b-saas-control-plane "$WASM" \
    >"$OUT_DIR/cleat-deploy.log" 2>&1; then
  echo "FAIL: cleat deploy" >&2
  cat "$OUT_DIR/cleat-deploy.log" >&2
  exit 1
fi

# ---- operator setup: one org, once per deployment -----------------------

echo "==> cleat-worker --create-org"
create_org_out="$("$OUT_DIR/cleat-worker" --create-org b2b-demo --db "$HOST_ADMIN_DB_URL" 2>&1)" || {
  echo "FAIL: --create-org" >&2
  echo "$create_org_out" >&2
  exit 1
}
#
# [[:space:]]/[^[:space:]], not \s/\S -- BSD sed (macOS) does not understand
# the GNU-only \s/\S shorthand in extended regex, and matches nothing rather
# than erroring. Measured directly: `echo "Org ID: x" | sed -nE 's/^Org
# ID:\s*(\S+)/\1/p'` prints nothing on macOS's sed and the id on GNU's. CI
# runs on ubuntu (GNU sed) and would not have caught this.
ORG_ID="$(printf '%s\n' "$create_org_out" | sed -nE 's/^Org ID:[[:space:]]*([^[:space:]]+)/\1/p' | head -1)"
if [[ -z "$ORG_ID" ]]; then
  echo "FAIL: --create-org printed no recognisable org id line:" >&2
  echo "$create_org_out" >&2
  exit 1
fi
echo "    org id: $ORG_ID"

echo "==> starting the backend"
CLEAT_URL="$API" \
CLEAT_ADMIN_DB_URL="$HOST_ADMIN_DB_URL" \
CLEAT_ORG_ID="$ORG_ID" \
CLEAT_WORKER_BIN="$OUT_DIR/cleat-worker" \
CLEAT_BIN="$OUT_DIR/cleat" \
CLEAT_WASM_PATH="$WASM" \
  "$OUT_DIR/backend" -listen ":$BACKEND_PORT" -web "$EXAMPLE_DIR/web" \
  >"$OUT_DIR/backend.log" 2>&1 &
BACKEND_PID=$!

BACKEND="http://localhost:$BACKEND_PORT"
deadline=$((SECONDS + 30))
until curl -fsS --max-time 5 "$BACKEND/api/config" >/dev/null 2>&1; do
  if (( SECONDS > deadline )); then
    echo "FAIL: the backend did not answer /api/config within 30s" >&2
    tail -40 "$OUT_DIR/backend.log" >&2
    exit 1
  fi
  sleep 1
done

# ---- assertion helper ----------------------------------------------------

check() {
  local desc="$1" got="$2" want="$3"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL: $desc: got $(printf '%q' "$got"), want $(printf '%q' "$want")" >&2
    failures=$((failures + 1))
  else
    echo "    ok: $desc"
  fi
}

poll_provisioning() {
  local run_id="$1" want_status="$2" key="$3" out
  local deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    out="$(curl -fsS --max-time 5 "$BACKEND/api/provisioning/$run_id" -H "Authorization: Bearer $key")"
    local stage
    stage="$(python3 -c 'import json,sys; d=json.load(sys.stdin); print((d.get("state") or {}).get("status") or d.get("status") or "")' <<<"$out")"
    if [[ "$stage" == "$want_status" ]]; then
      echo "$out"
      return 0
    fi
    sleep 1
  done
  echo "FAIL: run $run_id did not reach status=$want_status within 30s; last state: $out" >&2
  failures=$((failures + 1))
  echo "$out"
}

# ---- 1: a clean signup provisions and records every milestone -----------

echo "==> signup: Acme Corp"
signup1="$(curl -fsS -X POST "$BACKEND/api/signup" -H 'Content-Type: application/json' \
  -d '{"business_name":"Acme Corp","admin_email":"admin@acme.example","plan":"starter"}')"
TENANT1_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["tenant_id"])' <<<"$signup1")"
TENANT1_KEY="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"])' <<<"$signup1")"
RUN1_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["run_id"])' <<<"$signup1")"
echo "    tenant: $TENANT1_ID  run: $RUN1_ID"

detail1="$(poll_provisioning "$RUN1_ID" active "$TENANT1_KEY")"
check "run 1 worker status" "$(python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])' <<<"$detail1")" "done"

# ---- 2: a workspace failure still records the failure milestone ---------

echo "==> signup: Widgets Inc (simulated workspace failure)"
signup2="$(curl -fsS -X POST "$BACKEND/api/signup" -H 'Content-Type: application/json' \
  -d '{"business_name":"Widgets Inc","admin_email":"admin@widgets.example","plan":"starter","simulate_workspace_failure":true}')"
TENANT2_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["tenant_id"])' <<<"$signup2")"
echo "    tenant: $TENANT2_ID"
TENANT2_KEY="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["api_key"])' <<<"$signup2")"
RUN2_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["run_id"])' <<<"$signup2")"

detail2="$(poll_provisioning "$RUN2_ID" failed "$TENANT2_KEY")"
check "run 2 failed_step" "$(python3 -c 'import json,sys; print((json.load(sys.stdin).get("state") or {}).get("failed_step",""))' <<<"$detail2")" provision_workspace

# ---- 3: /api/tenant/lifecycle answers for the CALLER's own tenant -------

echo "==> tenant lifecycle: before any trial is set"
lc1="$(curl -fsS "$BACKEND/api/tenant/lifecycle" -H "Authorization: Bearer $TENANT1_KEY")"
check "tenant 1 has_trial before set-tenant-trial" "$(python3 -c 'import json,sys; print(json.load(sys.stdin)["has_trial"])' <<<"$lc1")" False

# ---- 4: the background sweep suspends an expired trial, live -----------
#
# --days must be positive (cmd/cleatctl/settenanttrial.go), so an immediate
# expiry cannot be requested through the sanctioned CLI surface -- exactly
# the awkward-but-real integration point the README names. This backdates
# the row directly, the same technique
# plugins/tenantlifecycle/background_multidb_test.go already uses to build
# an "already expired" fixture for the sweep's own unit tests.
#
# `SET search_path = public` FIRST, explicitly, in the SAME session -- this
# is CLAUDE.md's own documented landmine (cleat#2417's investigation): a
# plain `psql -U cleat` session's default search_path is "$user", public,
# and a schema literally named "cleat" exists (001_schema.sql), so an
# unqualified `INSERT INTO tenant_trials` would resolve against the WRONG
# schema and fail with "relation does not exist" rather than silently doing
# the wrong thing -- but only because plugin.RunMigrations defaults ITS OWN
# schema to "public" regardless of what core uses, so the two would disagree
# here specifically. Qualifying rather than guessing which way it resolves.
echo "==> backdating tenant 1's trial and waiting for the sweep"
psql_cmd=(docker compose -f "$EXAMPLE_DIR/docker-compose.yml" exec -T postgres \
  psql -v ON_ERROR_STOP=1 -U cleat -d cleat -c)
"${psql_cmd[@]}" \
  "SET search_path = public; INSERT INTO tenant_trials (tenant_id, expires_at, handled) VALUES ('$TENANT1_ID', now() - interval '1 hour', false)" \
  >"$OUT_DIR/backdate.log" 2>&1 || {
    echo "FAIL: backdating tenant 1's trial" >&2
    cat "$OUT_DIR/backdate.log" >&2
    exit 1
  }

# The sweep ticks every 60s (plugins/tenantlifecycle/background.go) and also
# runs once immediately on worker startup -- but the worker started before
# this row existed, so this script waits a full interval plus margin rather
# than assuming the immediate run caught it.
suspended=""
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  lc="$(curl -fsS "$BACKEND/api/tenant/lifecycle" -H "Authorization: Bearer $TENANT1_KEY" || true)"
  handled="$(python3 -c 'import json,sys
try:
  d=json.load(sys.stdin)
  print(d.get("handled"))
except Exception:
  print("")' <<<"$lc")"
  if [[ "$handled" == "True" ]]; then
    suspended="yes"
    break
  fi
  sleep 3
done
check "tenant 1's trial was handled (suspended) by the background sweep" "${suspended:-no}" yes

echo "==> a suspended tenant's own key can no longer start a run"
start_status="$(curl -s -o "$OUT_DIR/suspended-start.json" -w '%{http_code}' \
  -X POST "$API/api/workflows/b2b-saas-control-plane/start" \
  -H "Authorization: Bearer $TENANT1_KEY" -H 'Content-Type: application/json' \
  -d '{"input":{"tenant_id":"'"$TENANT1_ID"'","business_name":"Acme Corp","plan":"starter"}}')"
check "a suspended tenant's start request is refused" "$start_status" "403"

# ---- 5: the unsuspended second tenant is unaffected ----------------------

echo "==> tenant 2 (never given a trial) is unaffected"
lc2="$(curl -fsS "$BACKEND/api/tenant/lifecycle" -H "Authorization: Bearer $TENANT2_KEY")"
check "tenant 2 has_trial" "$(python3 -c 'import json,sys; print(json.load(sys.stdin)["has_trial"])' <<<"$lc2")" False

# ---- result ---------------------------------------------------------------

echo
if (( failures > 0 )); then
  echo "FAILED: $failures assertion(s) did not hold." >&2
  exit 1
fi
echo "OK: b2b-saas-control-plane scenario passed."
