#!/usr/bin/env bash
#
# Run the integration-hub wedge's ADVERSARIAL half end to end: a tenant
# uploads its own WASM step through POST /api/definitions, and the sandbox
# must refuse it when it tries to read the host filesystem. cleat#2597.
#
# WHY THIS EXISTS, AND WHY IT IS A SEPARATE SCRIPT
#
# scripts/run-integration-hub-scenario.sh's own new "the wedge" section
# proves the mechanism WORKS: a tenant's own step, uploaded through
# POST /api/definitions, runs as a child of the operator's workflow and
# transforms real data, on all three dialects, through the full
# docker-compose deployment.
#
# That is a different claim from the one this script asserts. Working code
# is not yet a security guarantee -- the guarantee is that a MALICIOUS or
# merely careless tenant step cannot escape the sandbox, and asserting that
# needs an adversarial input, not a happy path. Bundling the two into one
# script would make the existing scenario's exit-code contract (about
# whether the harness itself is working) carry a claim it was never built to
# report, which is why they are kept apart: "does it work" and "does it
# hold" are different questions, with different failure semantics, and
# conflating them is how a security regression would end up reported as
# "the demo needs updating."
#
# WHICH GUARANTEE, NAMED RATHER THAN LEFT VAGUE
#
# docs/playbooks/integration-hub.md's wedge section states three: "no
# ambient filesystem, no network beyond the host functions you allow, and
# its durable calls go through the same recorder." This script demonstrates
# the FIRST one, because it is the one with a concrete, testable mechanism
# already in the tree: engine/wasi_policy.go lists WASI's path_open as
# wasiFatal -- "the family this policy most exists to refuse" -- and
# engine/wasi_policy_wasmtime.go traps any guest that imports it. The other
# two are real properties of the same sandbox but are not what this script
# measures; naming the one it does is the point of writing this comment
# rather than a bare "tests the sandbox."
#
# THE POSITIVE CONTROL IS NOT DECORATION. A scenario that only ever asserts
# "the malicious step fails" cannot tell a working sandbox from a worker
# that fails every workflow. examples/integration-hub/tenant-steps/normalize-order
# is a legitimate tenant transform, uploaded and run the same way, and it
# must succeed -- if it does not, this script's own FAIL for the malicious
# step is worthless.
#
# ONE ARM, NOT A DIALECT MATRIX, like the DBOS pair scenario's own reasoning:
# the guarantee under test is a property of the WASM host, not of SQL, so
# there is no dialect axis to diverge on.
#
# WHAT WOULD HAVE MADE THIS SCRIPT'S ASSERTION MEANINGLESS UNTIL IT WAS FIXED:
# cleat#2612/#2613. A malicious step that the sandbox correctly trapped used
# to be reported to its caller as status=done, result="ok" -- the same class
# of hole IMPROVEMENT-PLAN §3.71 already fixed for an out-of-memory guest,
# reached here by a different, previously-undemonstrated cause. Building
# this script IS how that was found; #2613 is a prerequisite for this
# script's FAIL branch ever being reachable, not merely a nice-to-have.
#
# EXIT STATUS
#
#   0  the sandbox held (malicious step failed) and the positive control
#      succeeded
#   1  a scenario reported the wrong outcome -- a finding about the sandbox
#   2  UNMEASURED -- a precondition (binaries, DSN, docker) was not met.
#      Never printed as though it were a passing scenario.
set -uo pipefail
set +e

cd "$(git rev-parse --show-toplevel)" || exit 2

CLEAT_BIN="${CLEAT_BIN:-$(pwd)/.bin/cleat}"
WORKER_BIN="${WORKER_BIN:-$(pwd)/.bin/cleat-worker}"
for bin in "$CLEAT_BIN" "$WORKER_BIN"; do
  if [[ ! -x "$bin" ]]; then
    echo "UNMEASURED: no binary at $bin. Build it first:" >&2
    echo "  go build -o .bin/cleat ./cmd/cleat" >&2
    echo "  go build -o .bin/cleat-worker ./cmd/cleat-worker" >&2
    exit 2
  fi
done

OWNER_DB="${CLEAT_TENANT_SANDBOX_OWNER_DB:-}"
if [[ -z "$OWNER_DB" ]]; then
  echo "UNMEASURED: CLEAT_TENANT_SANDBOX_OWNER_DB is not set (a superuser DSN)." >&2
  echo "This script needs a reachable Postgres; it does not start one itself." >&2
  exit 2
fi

free_port() {
  python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
}

API_PORT="$(free_port)"
API="http://127.0.0.1:$API_PORT"
APP_PASSWORD="cleat-tenant-sandbox-$$"
WORKER_LOG="$(mktemp -t ih-tenant-sandbox-worker-XXXXXX.log)"
WORKER_PID=""
OUT_DIR="$(mktemp -d)"

cleanup() {
  local rc=$?
  if (( rc != 0 )) && [[ -n "$WORKER_PID" ]]; then
    echo >&2
    echo "--- cleat-worker (tail) ---" >&2
    tail -60 "$WORKER_LOG" >&2 2>&1 || true
  fi
  [[ -n "$WORKER_PID" ]] && kill "$WORKER_PID" >/dev/null 2>&1
  rm -rf "$OUT_DIR" "$WORKER_LOG"
}
trap cleanup EXIT

echo "==> provisioning the cleat_app role"
if ! "$WORKER_BIN" -db "$OWNER_DB" -migrate-only >/tmp/ih-ts-migrate.log 2>&1; then
  echo "FAIL: -migrate-only against the owner DSN failed" >&2
  tail -25 /tmp/ih-ts-migrate.log >&2
  exit 1
fi

ALTER_DSN="$(python3 -c "
import sys
dsn = sys.argv[1]
# postgres://user:pass@host:port/db?query -> the (host,port,db,query) part
after_at = dsn.split('@', 1)[-1]
print(after_at)
" "$OWNER_DB")"
# The owner DSN reused with psql-less role provisioning: connect through the
# worker binary's own migration path is not available for arbitrary SQL, so
# this uses psql if present and falls back to python3+psycopg-less raw SQL
# over the wire is not worth building here -- require psql, the same
# precondition every dialect scenario already has for its database image.
if ! command -v psql >/dev/null 2>&1; then
  echo "UNMEASURED: psql is not available to provision the cleat_app role." >&2
  exit 2
fi
if ! psql "$OWNER_DB" -c "ALTER ROLE cleat_app LOGIN PASSWORD '$APP_PASSWORD';" >/tmp/ih-ts-alter.log 2>&1; then
  echo "FAIL: ALTER ROLE cleat_app LOGIN failed" >&2
  tail -25 /tmp/ih-ts-alter.log >&2
  exit 1
fi

APP_DB="postgres://cleat_app:$APP_PASSWORD@$ALTER_DSN"

echo "==> starting the worker on port $API_PORT"
"$WORKER_BIN" -db "$APP_DB" -migrate-db "$OWNER_DB" -api-addr ":$API_PORT" >"$WORKER_LOG" 2>&1 &
WORKER_PID=$!

deadline=$((SECONDS + 60))
up=0
while (( SECONDS < deadline )); do
  if ! kill -0 "$WORKER_PID" >/dev/null 2>&1; then
    echo "FAIL: the worker process exited before it ever answered" >&2
    tail -40 "$WORKER_LOG" >&2
    exit 1
  fi
  if curl -fsS --max-time 3 "$API/healthz" >/dev/null 2>&1; then
    up=1
    break
  fi
  sleep 1
done
if [[ "$up" -ne 1 ]]; then
  echo "FAIL: the worker never answered /healthz within 60s" >&2
  tail -40 "$WORKER_LOG" >&2
  exit 1
fi

echo "==> provisioning an org, a tenant and an API key"
ORG_ID="$("$WORKER_BIN" -db "$OWNER_DB" -create-org "tenant-sandbox-org-$$" 2>&1 |
  grep -oE 'Org ID:\s+\S+' | awk '{print $3}')"
[[ -n "$ORG_ID" ]] || { echo "FAIL: -create-org produced no org id" >&2; exit 1; }
TENANT_ID="$("$WORKER_BIN" -db "$OWNER_DB" -create-tenant "tenant-sandbox-tenant-$$" -org "$ORG_ID" 2>&1 |
  grep -oE 'Tenant ID:\s+\S+' | awk '{print $3}')"
[[ -n "$TENANT_ID" ]] || { echo "FAIL: -create-tenant produced no tenant id" >&2; exit 1; }
API_KEY="$("$WORKER_BIN" -db "$OWNER_DB" -generate-api-key "$TENANT_ID" 2>&1 |
  grep -oE 'Key:\s+\S+' | awk '{print $2}')"
[[ -n "$API_KEY" ]] || { echo "FAIL: -generate-api-key produced no key" >&2; exit 1; }
auth=(-H "Authorization: Bearer $API_KEY")

failures=0

# upload_and_start builds a tenant-steps/<dir> package, uploads it through
# POST /api/definitions under the tenant's OWN key, starts it, and prints the
# run id.
upload_and_start() {
  local dir="$1" name="$2" entry="$3" input_json="${4:-}"
  [[ -z "$input_json" ]] && input_json='{}'
  local build_dir="$OUT_DIR/$name"
  mkdir -p "$build_dir"
  if ! "$CLEAT_BIN" build --target go -o "$build_dir" "./examples/integration-hub/tenant-steps/$dir/" \
      >/tmp/ih-ts-build-"$name".log 2>&1; then
    echo "FAIL: building $name failed" >&2
    tail -25 /tmp/ih-ts-build-"$name".log >&2
    return 1
  fi
  local wasm="$build_dir/$entry.wasm"
  if [[ ! -f "$wasm" ]]; then
    echo "FAIL: building $name did not produce $wasm" >&2
    ls -la "$build_dir" >&2
    return 1
  fi
  local def_json_file="$OUT_DIR/$name-def.json"
  # --data @file, NOT -d "$(...)": the JSON body is several MB once the wasm
  # is base64-encoded, and passing it as a single shell/exec argument risks
  # E2BIG ("Argument list too long") -- measured hitting it here.
  base64 < "$wasm" | tr -d '\n' | python3 -c '
import json, sys
print(json.dumps({"name": sys.argv[1], "wasm_bytes_base64": sys.stdin.read()}))
' "$name" >"$def_json_file"
  if ! curl -fsS --max-time 30 -X POST "$API/api/definitions" "${auth[@]}" \
      -H "Content-Type: application/json" --data @"$def_json_file" >/tmp/ih-ts-upload-"$name".log 2>&1; then
    echo "FAIL: POST /api/definitions for $name was refused" >&2
    cat /tmp/ih-ts-upload-"$name".log >&2
    return 1
  fi
  curl -fsS --max-time 15 -X POST "$API/api/workflows/$name/start" "${auth[@]}" \
    -H "Content-Type: application/json" -d "{\"input\":$input_json}" |
    python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))'
}

wait_for_terminal() {
  local id="$1" limit="${2:-30}"
  local until=$((SECONDS + limit))
  local status
  while (( SECONDS < until )); do
    status="$(curl -fsS --max-time 5 "$API/api/workflows/$id" "${auth[@]}" 2>/dev/null |
      python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("status",""))
except Exception: print("")')"
    case "$status" in
      done|failed|dead_lettered|cancelled|terminated) echo "$status"; return 0 ;;
    esac
    sleep 1
  done
  echo "$status"
  return 1
}

# ---- the positive control: a legitimate tenant transform must succeed ----
echo
echo "==> positive control: a legitimate tenant step (normalize-order)"
NORM_RUN="$(upload_and_start "normalize-order" "normalize-order" "normalize_order" \
  '{"order_id":"ord-tenant-sandbox-1","vendor_name":"acme"}')"
if [[ -z "$NORM_RUN" ]]; then
  echo "FAIL: the legitimate tenant step never started" >&2
  failures=$((failures + 1))
else
  status="$(wait_for_terminal "$NORM_RUN" 30)"
  if [[ "$status" == "done" ]]; then
    echo "    ok      normalize-order completed: status=done"
  else
    echo "FAIL: the legitimate tenant step reports status=$status, want done" >&2
    echo "        if a working tenant step cannot succeed, the FAIL below proves nothing" >&2
    failures=$((failures + 1))
  fi
fi

# ---- the adversarial case: a malicious tenant step must be refused ----
echo
echo "==> the wedge holds: a malicious tenant step (malicious-read-host-file)"
MAL_RUN="$(upload_and_start "malicious-read-host-file" "read-host-file" "read_host_file")"
if [[ -z "$MAL_RUN" ]]; then
  echo "FAIL: the malicious tenant step never started" >&2
  failures=$((failures + 1))
else
  status="$(wait_for_terminal "$MAL_RUN" 30)"
  if [[ "$status" != "failed" ]]; then
    echo "FAIL: the malicious tenant step reports status=$status, want failed" >&2
    echo "      a tenant-uploaded step that reads the host filesystem must not" >&2
    echo "      succeed, whatever it read." >&2
    failures=$((failures + 1))
  else
    err="$(curl -fsS --max-time 10 "$API/api/workflows/$MAL_RUN" "${auth[@]}" 2>/dev/null |
      python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("error",""))
except Exception: print("")')"
    if [[ "$err" == *'cleat refuses the WASI call "path_open"'* ]]; then
      echo "    ok      status=failed, refused by the WASI policy (path_open)"
    else
      echo "FAIL: status=failed, but not for the documented reason" >&2
      echo "      error: $err" >&2
      echo "      a different error means this is no longer exercising the" >&2
      echo "      filesystem-refusal guarantee." >&2
      failures=$((failures + 1))
    fi
  fi
fi

if (( failures > 0 )); then
  echo >&2
  echo "$failures assertion(s) failed" >&2
  exit 1
fi
echo
echo "all assertions passed"
