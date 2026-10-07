#!/usr/bin/env bash
#
# Run examples/order-lifecycle-dbos-port end to end against a real DBOS
# runtime and a real PostgreSQL, over HTTP -- the DBOS-side twin of
# scripts/run-order-lifecycle-scenario.sh. cleat#2597.
#
# WHY THIS EXISTS
#
# The DBOS port's own README states the claim this pair carries: a genuinely
# executed DBOS counterpart exists, comparable to cleat's by the same line
# counter, carrying the same approval gate and query state (cleat#2997). An
# unexecuted port is not evidence for
# that claim -- see the measurement doc that started this direction,
# cleat-internal/cleat-vs-dbos-build-and-operate-2026-09-28.md, which found
# the DBOS side looking smaller BEFORE anyone had run it. `npm test`
# (src/order.test.ts) already exercises the workflow and compensation logic
# directly; this script exercises the OTHER half -- the HTTP backend
# (src/server.ts) -- the same way the cleat-side scenario drives cleat's
# backend rather than only unit-testing the workflow package.
#
# WHAT IT ASSERTS, driven here through the HTTP surface (the workflow-level
# equivalents are in order.test.ts):
#
#   1. a confirmed payment with a clean reservation ships;
#   2. a declined charge (simulatePaymentFailure) never reaches reservation
#      and reports "declined";
#   3. a charge that completed and then failed to reserve unwinds and reports
#      "compensated";
#   4. an OVER-THRESHOLD order that is approved ships (cleat#2997);
#   5. an OVER-THRESHOLD order that is rejected reports "rejected" and spends
#      nothing (cleat#2997).
#
# Note that this list has five entries and the script asserts five. Until
# cleat#2997 it said four while asserting three: the compensation_failed case,
# which needs the second workflow entry point, is covered by order.test.ts and
# never was an HTTP scenario here.
#
# NOT A DIALECT MATRIX. cleat's own scenario runs three arms because this
# project's defect history is concentrated in SQL dialect divergence
# (CLAUDE.md, "WHY THREE ARMS AND NOT ONE"). DBOS Transact's Postgres system
# database is the only backend the SDK supports; there is no second dialect
# to diverge on, so one arm is the whole claim, not a narrowed one.
#
# EXIT STATUS
#
#   0  every scenario asserted correctly
#   1  a scenario reported the wrong status -- a finding about the port
#   2  UNMEASURED -- a precondition (node, npm, the built server) was not
#      met. Never printed as though it were scenario 0.
set -uo pipefail

# ERREXIT OFF, EXPLICITLY. See scripts/run-order-lifecycle-scenario.sh's
# identical declaration for why: several checks below read a non-zero exit
# (a curl timeout, a mismatched status) as DATA, not as a reason to abort the
# whole script before the failure counter can record it.
set +e

cd "$(git rev-parse --show-toplevel)" || exit 2

EXAMPLE_DIR="examples/order-lifecycle-dbos-port"

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
SERVER_LOG="$(mktemp -t ol-dbos-server-XXXXXX.log)"
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
if ! npm install --no-audit --no-fund >/tmp/ol-dbos-npm-install.log 2>&1; then
  echo "FAIL: npm install" >&2
  tail -30 /tmp/ol-dbos-npm-install.log >&2
  exit 1
fi

echo "==> npm run build"
if ! npm run build >/tmp/ol-dbos-npm-build.log 2>&1; then
  echo "FAIL: npm run build" >&2
  tail -30 /tmp/ol-dbos-npm-build.log >&2
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
  echo "FAIL: the server never answered POST /orders within 60s" >&2
  tail -40 "$SERVER_LOG" >&2
  exit 1
fi

failures=0

place_order() {
  # $1: orderId  $2..: extra JSON fields, each "key:jsonvalue"
  local order_id="$1"; shift
  local extra=""
  for kv in "$@"; do
    extra="${extra}, \"${kv%%:*}\": ${kv#*:}"
  done
  curl -fsS --max-time 10 -X POST "$API/orders" \
    -H 'Content-Type: application/json' \
    -d "{\"orderId\":\"$order_id\",\"customerId\":\"cust-1\",\"email\":\"a@example.com\",\"items\":[{\"sku\":\"sku-1\",\"quantity\":2,\"priceCents\":500}]${extra}}"
}

confirm_payment() {
  curl -fsS --max-time 10 -X POST "$API/webhooks/payment/$1" \
    -H 'Content-Type: application/json' -d '{"event":"payment-confirmed"}' >/dev/null
}

# place_expensive_order is place_order with one item ONE CENT ABOVE the
# workflow's approval threshold (50_000), so the order parks on its approval
# gate. The window is short (5s) so the script is never the thing that makes a
# run slow; the approval scenarios below decide explicitly rather than waiting
# it out.
place_expensive_order() {
  local order_id="$1"
  curl -fsS --max-time 10 -X POST "$API/orders" \
    -H 'Content-Type: application/json' \
    -d "{\"orderId\":\"$order_id\",\"customerId\":\"cust-1\",\"email\":\"a@example.com\",\"items\":[{\"sku\":\"server\",\"quantity\":1,\"priceCents\":50001}],\"approvalWindowSeconds\":5}"
}

# decide delivers the human decision -- the DBOS counterpart of cleat's
# POST /api/orders/{id}/approve, which signals order_approved/order_rejected.
# Here it is one topic carrying the decision, because DBOS has no
# multi-signal wait; see DECISION_TOPIC in src/workflow.ts.
decide() {
  local wf="$1" approve="$2" reason="${3:-}"
  curl -fsS --max-time 10 -X POST "$API/orders/$wf/approve" \
    -H 'Content-Type: application/json' \
    -d "{\"approve\":${approve},\"reason\":\"${reason}\"}" >/dev/null
}

poll_status() {
  # $1: workflowID  $2: field name to read with python
  local wf="$1" field="$2"
  local deadline=$((SECONDS + 30))
  local status=""
  while (( SECONDS < deadline )); do
    status="$(curl -fsS --max-time 5 "$API/orders/$wf" 2>/dev/null | \
      python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('$field',''))" 2>/dev/null)"
    if [[ -n "$status" && "$status" != "PENDING" && "$status" != "ENQUEUED" ]]; then
      echo "$status"
      return 0
    fi
    sleep 1
  done
  echo "$status"
}

assert_status() {
  local scenario="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then
    echo "ok: $scenario -> $got"
  else
    echo "FAIL: $scenario -- got '$got', want '$want'" >&2
    failures=$((failures + 1))
  fi
}

echo
echo "==> scenario: happy path"
resp="$(place_order order-http-happy)"
wf="$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["workflowID"])')"
confirm_payment "$wf"
status="$(poll_status "$wf" status)"
assert_status "happy path" "$status" "shipped"

echo
echo "==> scenario: declined (no confirmation)"
resp="$(place_order order-http-declined 'simulatePaymentFailure:true')"
wf="$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["workflowID"])')"
status="$(poll_status "$wf" status)"
assert_status "declined" "$status" "declined"

echo
echo "==> scenario: reservation fails after charge (compensated)"
resp="$(place_order order-http-compensated 'simulateFulfilmentFailure:true')"
wf="$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["workflowID"])')"
confirm_payment "$wf"
status="$(poll_status "$wf" status)"
assert_status "reservation fails after charge" "$status" "compensated"

echo
echo "==> scenario: over-threshold order is approved, then ships"
resp="$(place_expensive_order order-http-approved)"
wf="$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["workflowID"])')"
decide "$wf" true
confirm_payment "$wf"
status="$(poll_status "$wf" status)"
assert_status "approved over-threshold order" "$status" "shipped"

echo
echo "==> scenario: over-threshold order is rejected and spends nothing"
resp="$(place_expensive_order order-http-rejected)"
wf="$(echo "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["workflowID"])')"
decide "$wf" false "over budget"
status="$(poll_status "$wf" status)"
assert_status "rejected over-threshold order" "$status" "rejected"

if (( failures > 0 )); then
  echo >&2
  echo "$failures scenario(s) failed" >&2
  exit 1
fi
echo
echo "all scenarios passed"
