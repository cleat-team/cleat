#!/usr/bin/env bash
#
# Run examples/integration-hub end to end against a real worker, KILL the worker
# mid-run, and assert that the resumed run did not dispatch twice.
#
# WHY THE KILL IS THE TEST, AND NOT AN EXTRA
#
# The property this scenario exists to demonstrate is that a host call is
# RECORDED in event history and replayed deterministically -- which is what
# separates cleat from a plugin system. `notifications.send_webhook` is
# registered `Idempotent: false` (plugins/notifications/host_functions.go), so
# if the engine did not record the call, a worker that died after making it
# would make it again on resume, and the customer's CRM would receive the same
# event twice.
#
# A green run that never crashes does not test any of that. So:
#
#   1. start a sync, deliver the inbound event, and WAIT until the connector
#      dispatch has happened -- observed as a delivery row appearing;
#   2. SIGKILL the worker, in the settle window between the dispatch and the
#      end of the run;
#   3. restart it and let the run finish;
#   4. assert the count.
#
# WHAT THE COUNT IS, AND WHY IT IS THE ROW RATHER THAN THE SINK
#
# `send_webhook` does not deliver inline: it writes a 'pending' delivery row and
# returns its id, and the plugin's own background loop performs the HTTP
# request. So a re-executed call is a SECOND ROW -- durable, countable through
# GET /webhooks/{id}/deliveries, and independent of HTTP timing.
#
# AND THE SINK IS WHERE IT ACTUALLY LANDS, which is a separate fact from the row
# and is asserted separately. A delivery row says the dispatch was RECORDED; a
# hit at the sink says the background loop then PICKED IT UP and the HTTP request
# left the worker. Two instruments, two questions, and the second is what closes
# the background-loop hitch point.
#
# The sink is PRIVATE and the delivery lands anyway, because the compose grants
# exactly one host by name:
#
#   --plugin-egress-allow-private=sink
#
# Without it the delivery is refused -- "egress to sink (172.19.0.3) is refused
# by cleat's network policy: RFC1918 private" -- which reads like a closed door
# and is a door with a handle. `engine/egress_policy.go` marks the RFC1918
# prefixes `exemptible: true`. Permitting the one host is not a convenience for
# this example: an integration hub's whole job is reaching systems inside the
# customer's network, so "make your rope end publicly reachable" is the opposite
# of the architecture. See the example's README.
#
# DERIVED, NOT LISTED
#
# The two `cleat` commands come out of the example's README verbatim, the way
# build-documented-examples.sh takes its `cleat build`. See
# run-order-lifecycle-scenario.sh for the substitutions and why they are not a
# lie about the README.
#
# THE FOUR HITCH POINTS ARE EACH ASSERTED, not merely present:
#
#   routes            the signed POST to /ingest/{source_id} is what the run
#                     waits on, and the run does not proceed without it
#   host functions    the delivery row, and the crash-resume count above all
#   edge middleware   a limit seeded through the PLUGIN's own route, then a
#                     burst, then a check that the 429 carried the plugin's
#                     X-RateLimit-Limit and not the core limiter's bare 429
#   background loop   the loop picks the pending row up, makes the request, and
#                     the sink records it -- plus a KNOWN POSITIVE below, because
#                     a count that can only ever read 1 would pass the
#                     crash-resume assertion identically
set -uo pipefail

# ERREXIT OFF, EXPLICITLY -- as a DECLARATION, not as a fix for anything.
#
# Several steps here read a non-zero exit as DATA rather than as failure, and
# the wait helpers loop until a condition holds rather than asserting it on the
# first try. This line says the script is not written for errexit rather than
# leaving that to the invoker.
#
# IT IS A NO-OP UNDER CI, AND A CHILD BASH DOES NOT INHERIT ERREXIT -- measured:
# `bash -e parent.sh` gives the child `$- = hB` and a SHELLOPTS without errexit,
# and the child continues past `false`. So this matters for `bash -e <this
# file>` and for sourcing, and for stating the intent where it is otherwise
# invisible. Stating it is cheap; depending on the invoker's shell is not.
set +e

cd "$(git rev-parse --show-toplevel)" || exit 2

EXAMPLE_DIR="examples/integration-hub"
README="$EXAMPLE_DIR/README.md"

CLEAT_BIN="${CLEAT_BIN:-$(pwd)/.bin/cleat}"
if [[ ! -x "$CLEAT_BIN" ]]; then
  echo "UNMEASURED: no cleat binary at $CLEAT_BIN. Build it first:" >&2
  echo "  go build -o .bin/cleat ./cmd/cleat" >&2
  exit 2
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "UNMEASURED: docker is not available, so nothing here can be run." >&2
  echo "This is a failure of the job, not a finding about the example." >&2
  exit 2
fi

# ---- the dialect -------------------------------------------------------
#
# scripts/run-integration-hub-scenario.sh [postgres|mysql|mssql]
#
# cleat#2560. Same shape as scripts/run-order-lifecycle-scenario.sh -- read that
# script's DIALECT ARMS note, which is the reference; this file states only what
# differs.
#
# WHAT DIFFERS HERE: this scenario polls the notifications plugin's deliveries
# route, and THAT route is dialect-parameterised where order-lifecycle's saga is
# not. plugins/notifications/routes.go crosses four seams on the path this file
# exercises:
#
#   webhookExistsSQL(d)  -- `SELECT EXISTS(...)` is invalid on SQL Server, so the
#                           statement itself is different there (routes.go:69)
#   plugin.LimitClause   -- SQL Server has no LIMIT              (routes.go:574)
#   plugin.Rebind        -- $N vs ?, by appearance vs by number  (routes.go:540)
#   plugin.ScanRow       -- column scanning                      (routes.go:596)
#
# That endpoint's own comment records what it cost the first time it met SQL
# Server: "every call failed outright". So this half is the reason a dialect arm
# is worth more here than on the saga -- the deliveries assertions below are what
# would have caught it.
DIALECT="${1:-postgres}"
case "$DIALECT" in
  postgres)
    PORT_ENV=CLEAT_PG_PORT
    HOST_DB_URL_TMPL='postgres://cleat:cleat@localhost:__PORT__/cleat?sslmode=disable'
    NET_MIGRATE_DB_URL='postgres://cleat:cleat@postgres:5432/cleat?sslmode=disable'
    NET_WORKER_DB_URL='postgres://cleat_app:cleat-app-local-dev@postgres:5432/cleat?sslmode=disable'
    ;;
  mysql)
    PORT_ENV=CLEAT_MYSQL_PORT
    HOST_DB_URL_TMPL='root:cleat@tcp(localhost:__PORT__)/cleat?tls=false&parseTime=true'
    NET_MIGRATE_DB_URL='root:cleat@tcp(mysql:3306)/cleat?tls=false&parseTime=true'
    NET_WORKER_DB_URL="$NET_MIGRATE_DB_URL"
    ;;
  mssql)
    PORT_ENV=CLEAT_MSSQL_PORT
    HOST_DB_URL_TMPL='sqlserver://sa:CleatTest123!@localhost:__PORT__?database=cleat'
    NET_MIGRATE_DB_URL='sqlserver://sa:CleatTest123!@mssql:1433?database=cleat'
    NET_WORKER_DB_URL="$NET_MIGRATE_DB_URL"
    ;;
  *)
    echo "UNMEASURED: unknown dialect '$DIALECT'." >&2
    echo "Expected one of: postgres, mysql, mssql. Nothing was run." >&2
    exit 2
    ;;
esac

# ---- extract the documented commands ----------------------------------
#
# Join line-continuations BEFORE looking for anything: a command written across
# two lines is one command, and a line-oriented scan reads the second half as a
# separate -- and invalid -- invocation.
DOC_COMMANDS="$(awk '{ while (sub(/\\$/, "")) { if ((getline nxt) > 0) $0 = $0 nxt; else break } print }' "$README" |
  grep -vE '^[[:space:]]*#' | sed 's/^[[:space:]]*\$ //')"

extract_one() { printf '%s\n' "$DOC_COMMANDS" | grep -m1 -E "$1" || true; }

BUILD_CMD="$(extract_one '(^| )cleat build ')"
DEPLOY_CMD="$(extract_one '(^| )cleat deploy ')"
DEPLOY_WF_CMD="$(extract_one 'deploy-workflow --driver')"

# The deploy step is TWO documented commands, one per group of dialects -- the
# `cleat` CLI is PostgreSQL-only and refuses a MySQL or SQL Server DSN, naming
# `deploy-workflow` as the alternative. So the required set is dialect-dependent
# rather than loosened to "at least one of these", which would pass an arm that
# had quietly stopped finding its own command.
if [[ "$DIALECT" == "postgres" ]]; then
  REQUIRED_CMDS=(build deploy)
  DEPLOY_LABEL=deploy
else
  REQUIRED_CMDS=(build deploy-workflow)
  DEPLOY_LABEL=deploy-workflow
fi

examined=0
for want in "${REQUIRED_CMDS[@]}"; do
  case "$want" in
    build)           [[ -n "$BUILD_CMD" ]]     && examined=$((examined + 1)) ;;
    deploy)          [[ -n "$DEPLOY_CMD" ]]    && examined=$((examined + 1)) ;;
    deploy-workflow) [[ -n "$DEPLOY_WF_CMD" ]] && examined=$((examined + 1)) ;;
  esac
done
if (( examined < ${#REQUIRED_CMDS[@]} )); then
  echo "UNMEASURED: found $examined of ${#REQUIRED_CMDS[@]} documented commands for the $DIALECT arm (${REQUIRED_CMDS[*]}) in $README." >&2
  echo "The extractor stopped seeing them, or the README stopped documenting them. Either" >&2
  echo "way a clean result below would mean nothing. Extracted:" >&2
  printf '  build           : %s\n  deploy          : %s\n  deploy-workflow : %s\n' \
    "$BUILD_CMD" "$DEPLOY_CMD" "$DEPLOY_WF_CMD" >&2
  exit 2
fi

# ---- environment -------------------------------------------------------

free_port() {
  python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
}

SUFFIX="$$-$(date +%s)"
DB_PORT="$(free_port)"
API_PORT="$(free_port)"
WORKER_IMAGE="cleat-integration-hub-test:$SUFFIX"
COMPOSE=(docker compose --profile "$DIALECT" -f "$EXAMPLE_DIR/docker-compose.yml")
OUT_DIR="$(mktemp -d)"
# The port is chosen at runtime, so the host DSN is templated rather than
# literal. CLEAT_DB_URL is the variable the README's dialect table tells a reader
# to set, so both documented deploy commands work unchanged on every dialect.
HOST_DB_URL="${HOST_DB_URL_TMPL//__PORT__/$DB_PORT}"
export "$PORT_ENV=$DB_PORT" CLEAT_API_PORT="$API_PORT"
export CLEAT_DIALECT="$DIALECT"
export CLEAT_DB_URL="$HOST_DB_URL"
export CLEAT_MIGRATE_DB_URL="$NET_MIGRATE_DB_URL" CLEAT_WORKER_DB_URL="$NET_WORKER_DB_URL"
export COMPOSE_PROJECT_NAME="cleat-integration-hub-$SUFFIX"

# The failure counter, initialised ONCE, above every check that can increment it.
# Moving it here rather than beside the first check is deliberate: a counter
# placed relative to an editing history is a state that decays, and this one did
# -- twice in the sibling script, by the same route, three edits apart.
failures=0

# Where the non-PostgreSQL deploy binary is built. The README's second deploy
# command names this exact path, so building it here lets the extracted command
# run verbatim rather than being rewritten.
DEPLOY_WF_BIN="$OUT_DIR/deploy-workflow"

# On failure the worker's own log is the only artefact that explains what
# happened, and the stack is about to be removed -- so it is captured HERE. A
# separate CI step for this would find nothing: by the time it runs, the
# containers are gone.
cleanup() {
  local rc=$?
  if (( rc != 0 )); then
    echo >&2
    # SINCE THE RESTART WHEN THERE WAS ONE, and that is the difference between
    # evidence and a red herring. This dumped `--tail=60` unconditionally until
    # 2026-09-28, and after a restart those 60 lines are the pre-kill startup
    # burst -- so the failure that mattered was reported with a perfectly
    # healthy worker's log attached to it (cleat#2562's CI run).
    if [[ -n "${RESTART_AT:-}" ]]; then
      echo "--- cleat-worker (since the $RESTART_AT restart) ---" >&2
      "${COMPOSE[@]}" logs --since "$RESTART_AT" cleat-worker >&2 2>&1 || true
    else
      echo "--- cleat-worker (tail) ---" >&2
      "${COMPOSE[@]}" logs --tail=60 cleat-worker >&2 2>&1 || true
    fi
    echo "--- sink (tail) ---" >&2
    "${COMPOSE[@]}" logs --tail=10 sink >&2 2>&1 || true
  fi
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  docker rmi -f "$WORKER_IMAGE" >/dev/null 2>&1 || true
  rm -rf "$OUT_DIR"
}
trap cleanup EXIT

default_image_count="$(grep -c 'CLEAT_WORKER_IMAGE:-ghcr.io/cleat-team/cleat-worker:latest' "$EXAMPLE_DIR/docker-compose.yml")"
if [[ "$default_image_count" -ne 2 ]]; then
  echo "FAIL: docker-compose.yml names the published worker image as its default" >&2
  echo "      $default_image_count times, want 2 (migrate and cleat-worker)." >&2
  exit 1
fi
export CLEAT_WORKER_IMAGE="$WORKER_IMAGE"

# The non-PostgreSQL deploy tool, built the way the README tells a reader to
# build it. A PRECONDITION rather than one of the counted commands, for the same
# reason `.bin/cleat` is: it is tooling, not the scenario.
if [[ "$DIALECT" != "postgres" ]]; then
  echo "==> building deploy-workflow from this checkout"
  if ! go build -o "$DEPLOY_WF_BIN" ./cmd/deploy-workflow >/tmp/ih-dw-build.log 2>&1; then
    echo "FAIL: the README's documented build of deploy-workflow failed:" >&2
    tail -20 /tmp/ih-dw-build.log >&2
    exit 1
  fi
fi

echo "==> building the worker image from this checkout"
if ! docker build -t "$WORKER_IMAGE" . >/tmp/ih-docker-build.log 2>&1; then
  echo "FAIL: docker build of the repository's Dockerfile" >&2
  tail -25 /tmp/ih-docker-build.log >&2
  exit 1
fi

echo "==> docker compose up"
if ! "${COMPOSE[@]}" up -d >/tmp/ih-compose-up.log 2>&1; then
  echo "FAIL: docker compose up" >&2
  tail -30 /tmp/ih-compose-up.log >&2
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

API_KEY=""
deadline=$((SECONDS + 30))
while (( SECONDS < deadline )); do
  API_KEY="$("${COMPOSE[@]}" logs cleat-worker 2>/dev/null | grep -oE 'cleat_sk_[0-9a-f]+' | head -1 || true)"
  [[ -n "$API_KEY" ]] && break
  sleep 1
done
if [[ -z "$API_KEY" ]]; then
  echo "FAIL: the worker printed no API key; nothing below can authenticate." >&2
  "${COMPOSE[@]}" logs --tail=60 cleat-worker >&2 || true
  exit 1
fi
auth=(-H "Authorization: Bearer $API_KEY")

# Announced here so the dialect is on screen before any output a reader might
# mistake for the default arm's. The RLS check itself is in the assertions
# section beside `contains`, because a bash function must be defined before it is
# called and the helpers are all defined below this point.
echo
echo "==> dialect: $DIALECT"

# ---- the documented commands -------------------------------------------

ran=0
# Derived from the dialect's own required set, not hardcoded: the two arms run
# two documented commands each, but they are not the same two.
total=${#REQUIRED_CMDS[@]}

run_documented() {
  local label="$1" cmd="$2"
  echo
  echo "==> ($label) \$ $cmd"
  case "$cmd" in
    *"$CLEAT_BIN"*|*"$DEPLOY_WF_BIN"*) ;;
    *) echo "FAIL: the extracted $label command runs neither $CLEAT_BIN nor $DEPLOY_WF_BIN: $cmd" >&2; exit 1 ;;
  esac
  if ! eval "$cmd" >/tmp/ih-cmd.log 2>&1; then
    echo "FAIL: the README's documented command failed:" >&2
    tail -25 /tmp/ih-cmd.log >&2
    exit 1
  fi
  tail -3 /tmp/ih-cmd.log
  ran=$((ran + 1))
}

BUILD_RUN="${BUILD_CMD//cleat /$CLEAT_BIN }"
BUILD_RUN="${BUILD_RUN//-o \/tmp\/out/-o $OUT_DIR}"
run_documented build "$BUILD_RUN"

WASM="$OUT_DIR/integration-hub.wasm"
if [[ ! -f "$WASM" ]]; then
  echo "FAIL: the documented build did not produce $WASM." >&2
  echo "The artifact is named for cleat.yaml's own 'name:' (cleat#2692), not the directory." >&2
  ls -la "$OUT_DIR" >&2
  exit 1
fi

# ---- the deploy step, which is the ONE command whose text differs ---------
#
# `cleat` is PostgreSQL-only: cmd/cleat/db.go refuses a MySQL or SQL Server DSN
# and names `deploy-workflow` as the alternative, which is why the README
# documents two deploy commands rather than one. The non-PostgreSQL arms take the
# second. Measured on the sibling scenario, 2026-09-28: the arms failed HERE, on
# the README's own command, before the compose was ever reached.
if [[ "$DIALECT" == "postgres" ]]; then
  DEPLOY_RUN="${DEPLOY_CMD//cleat /$CLEAT_BIN }"
else
  DEPLOY_RUN="$DEPLOY_WF_CMD"
fi
# `/tmp/out` is the convention every other example README uses; redirect it to a
# directory this run owns. On the non-PostgreSQL arms this also rewrites
# `/tmp/out/deploy-workflow` to the binary built above.
#
# There was a third substitution here until the dialect arm, rewriting
# `localhost:5432` to the free port. It is gone because the README no longer
# names a port: both deploy forms read `--db "$CLEAT_DB_URL"`.
DEPLOY_RUN="${DEPLOY_RUN//\/tmp\/out/$OUT_DIR}"
run_documented "$DEPLOY_LABEL" "$DEPLOY_RUN"

# And the refusal itself, asserted rather than routed around.
#
# The CLI's PostgreSQL-only-ness is the fact that explains why there are two
# deploy commands, so it is checked here rather than described in a comment --
# and it makes the limitation visible if it is ever lifted: the day `cleat deploy`
# accepts a MySQL DSN, this goes red and says the README is stale.
if [[ "$DIALECT" != "postgres" ]]; then
  refuse_log="/tmp/ih-cli-refuse-$DIALECT.log"
  if "$CLEAT_BIN" deploy --db "$CLEAT_DB_URL" --name integration-hub "$WASM" \
       >"$refuse_log" 2>&1; then
    echo "    FAIL    cleat deploy ACCEPTED a $DIALECT DSN. This arm deploys with" >&2
    echo "            deploy-workflow because the README says the CLI is PostgreSQL-only." >&2
    echo "            One of the two is now wrong, and this arm's premise is the CLI one." >&2
    failures=$((failures + 1))
  elif grep -q 'only supports PostgreSQL' "$refuse_log"; then
    echo "    ok      cleat deploy refuses a $DIALECT DSN, naming PostgreSQL as the reason"
  else
    # A non-zero exit for some OTHER reason is not the documented behaviour and
    # must not read as a pass: MEASURED on the sibling scenario, `cleat deploy`
    # exits 1 for the dialect refusal AND exits 1 for a PostgreSQL DSN against a
    # closed port, so the status cannot separate them.
    echo "    FAIL    cleat deploy failed on $DIALECT, but not for the documented reason:" >&2
    sed 's/^/            /' "$refuse_log" >&2
    failures=$((failures + 1))
  fi
fi

# ---- setup: the sink, and the two registrations -------------------------
#
# Not documented `cleat` commands -- these are the HTTP calls the README shows,
# and they are the scenario's setup rather than its counted steps.

echo
echo "==> registering the connector (the rope end) and the ingest source"

WEBHOOK_JSON="$(curl -fsS --max-time 15 -X POST "$API/webhooks" "${auth[@]}" \
  -H "Content-Type: application/json" \
  -d '{"url":"http://sink:9099/hook","secret":"whsec_local_dev","events":["contact.updated"]}' 2>&1)" || {
    echo "FAIL: POST /webhooks: $WEBHOOK_JSON" >&2; exit 1; }
WEBHOOK_ID="$(printf '%s' "$WEBHOOK_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
[[ -n "$WEBHOOK_ID" ]] || { echo "FAIL: no webhook id: $WEBHOOK_JSON" >&2; exit 1; }
echo "    connector webhook ${WEBHOOK_ID:0:8}…"

SOURCE_JSON="$(curl -fsS --max-time 15 -X POST "$API/ingest/sources" "${auth[@]}" \
  -H "Content-Type: application/json" \
  -d '{"name":"crm","source_type":"crm","secret":"whsec_local_dev"}' 2>&1)" || {
    echo "FAIL: POST /ingest/sources: $SOURCE_JSON" >&2; exit 1; }
SOURCE_ID="$(printf '%s' "$SOURCE_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
[[ -n "$SOURCE_ID" ]] || { echo "FAIL: no source id: $SOURCE_JSON" >&2; exit 1; }
echo "    ingest source ${SOURCE_ID:0:8}…"

# ---- helpers -----------------------------------------------------------
#
# `failures` is NOT reset here. It was, and it was harmless -- nothing above this
# point incremented it -- which is exactly why it survived: the defect only
# appears once a check is added above it, and then it is silent. It is now
# initialised once near the top with the rest of the script's state, so no check
# can precede the initialisation whatever order this file is edited into.

run_status() {
  curl -fsS --max-time 10 "$API/api/workflows/$1" "${auth[@]}" 2>/dev/null |
    python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("status",""))
except Exception: print("")'
}

wait_for_status() {
  local id="$1" want="$2" limit="${3:-90}"
  local until=$((SECONDS + limit))
  while (( SECONDS < until )); do
    [[ "$(run_status "$id")" == "$want" ]] && return 0
    sleep 1
  done
  return 1
}

# worker_container_state prints docker compose's own view of cleat-worker's
# state on stdout ("running", "exited", "restarting", ... or "absent" if no
# container exists -- a real, determined state, not an error) and returns 0.
# When it could NOT determine one -- `ps` itself failing (daemon
# unreachable, a transient CLI error) or its output not parsing -- it
# prints nothing on stdout, a diagnosis on stderr, and returns 1.
#
# This is the ground truth `up -d`'s own exit code does not provide
# (cleat#2777): `up -d` returned 0 while cleat-worker sat Exited(137) from
# an earlier SIGKILL, never restarted -- compose can report success
# without the container ever coming up.
#
# The exit-status/stdout split above is cleat#2803's own review finding,
# fixed here rather than left as a trap for the next reader: an earlier
# version of this function used `2>/dev/null` and printed "" for every
# failure, indistinguishable from a container that genuinely does not
# exist. Since the in-loop caller below runs this roughly once a second for
# up to 180s, a single transient `ps` hiccup got reported as "NOT RUNNING
# ... never came up" -- #2777's own defect shape (a zero exit is not proof
# of a positive) recurring one layer down (an empty read is not proof of a
# negative). Callers must check the exit status, not just the string.
#
# `ps --format json` emits one JSON object per line on the Compose version
# this was verified against; an older Compose emitting a single JSON array
# is also accepted.
worker_container_state() {
  local out rc err
  out="$("${COMPOSE[@]}" ps -a --format json cleat-worker 2>/tmp/ih-ps-state.err)"
  rc=$?
  err="$(tr '\n' ' ' </tmp/ih-ps-state.err 2>/dev/null)"
  if (( rc != 0 )); then
    echo "worker_container_state: docker compose ps exited $rc: $err" >&2
    return 1
  fi
  local state
  state="$(python3 -c '
import json, sys
data = sys.stdin.read()
try:
    rows = json.loads(data) if data.strip().startswith("[") \
        else [json.loads(l) for l in data.splitlines() if l.strip()]
except Exception:
    sys.exit(1)
for d in rows:
    if isinstance(d, dict) and d.get("Service") == "cleat-worker":
        print(d.get("State", "absent"))
        sys.exit(0)
print("absent")
' <<<"$out")" || { echo "worker_container_state: could not parse docker compose ps output" >&2; return 1; }
  echo "$state"
}

# wait_for_worker_state polls worker_container_state for up to $limit
# seconds. It only succeeds on a DETERMINED match -- an unknown reading
# (worker_container_state returning 1) is neither a match nor a mismatch,
# so it is treated the same as "not yet the wanted state": keep polling,
# never fail fast on it. A caller that needs to tell "genuinely not
# running" apart from "could not tell" after this returns 1 must call
# worker_container_state itself for the final determination (see the two
# call sites below); this function's own job is only "did it reach $want".
wait_for_worker_state() {
  local want="$1" limit="${2:-15}"
  local until=$((SECONDS + limit))
  local state
  while (( SECONDS < until )); do
    if state="$(worker_container_state 2>/dev/null)" && [[ "$state" == "$want" ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# web_status and wait_for_web_status read the APP's view of a run rather than the
# worker's. They are separate from run_status on purpose: the backend is the
# thing being exercised in the last section, so going around it to the worker to
# ask whether it worked would answer a different question.
web_status() {
  curl -fsS --max-time 10 "$WEB/api/syncs/$1" 2>/dev/null |
    python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("status",""))
except Exception: print("")'
}

wait_for_web_status() {
  local id="$1" want="$2" limit="${3:-90}"
  local until=$((SECONDS + limit))
  while (( SECONDS < until )); do
    [[ "$(web_status "$id")" == "$want" ]] && return 0
    sleep 2
  done
  return 1
}

# delivery_count reads the connector's own record -- the durable artefact.
delivery_count() {
  curl -fsS --max-time 10 "$API/webhooks/$WEBHOOK_ID/deliveries" "${auth[@]}" 2>/dev/null |
    python3 -c 'import json,sys
try:
    d = json.load(sys.stdin)
    if isinstance(d, dict):
        d = d.get("deliveries", d.get("items", []))
    print(len(d))
except Exception:
    print("-1")'
}

# sink_hits counts what actually left the worker and arrived. `/tmp/hits` is the
# sink container's own file; a missing file is zero hits, not an error.
sink_hits() {
  "${COMPOSE[@]}" exec -T sink sh -c 'test -f /tmp/hits && wc -l < /tmp/hits || echo 0' 2>/dev/null |
    tr -d ' \r' | head -1
}

check() {
  local what="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then echo "    ok      $what = $got"
  else echo "    FAIL    $what = '$got', want '$want'" >&2; failures=$((failures + 1)); fi
}

contains() {
  local what="$1" hay="$2" needle="$3"
  if [[ "$hay" == *"$needle"* ]]; then echo "    ok      $what contains '$needle'"
  else echo "    FAIL    $what does not contain '$needle' (got: $hay)" >&2; failures=$((failures + 1)); fi
}

# ---- what THIS dialect does and does not guarantee ----------------------
#
# The one place the three arms are deliberately not the same assertion. Read the
# DIALECT ARMS note in the header before changing it.
#
# `contains` is reused for the PostgreSQL half because the claim there is a
# positive fact the worker prints. The other half has no fact to assert, and
# saying so -- without counting it as a pass -- is the point.
worker_log="$("${COMPOSE[@]}" logs cleat-worker 2>/dev/null || true)"
if [[ "$DIALECT" == "postgres" ]]; then
  contains "the worker's RLS posture" "$worker_log" \
    "row-level security is enforced on this connection"
else
  if [[ "$worker_log" == *"row-level security is enforced on this connection"* ]]; then
    # The gate in cmd/cleat-worker/main.go is `--driver == "postgres"`, and
    # engine.CheckRLSEnforced queries pg_roles. If this ever prints on another
    # dialect then the README's statement about that dialect -- that nothing
    # there enforces RLS -- has become false, whatever else changed.
    echo "    FAIL    the worker reports RLS enforced on $DIALECT, where the README" >&2
    echo "            says nothing does. The README's dialect table is now wrong." >&2
    failures=$((failures + 1))
  else
    echo "    --      no RLS assertion on $DIALECT, and none available to make."
    echo "            cleat's MySQL is single-tenant by construction and SQL Server has"
    echo "            no runtime enforcement check either: cmd/cleat-worker/main.go gates"
    echo "            the whole check on --driver == \"postgres\", before calling"
    echo "            engine.CheckRLSEnforced, which queries pg_roles and could not run"
    echo "            here. The assertions below are NOT weaker for it -- they are the"
    echo "            same assertions, and they are what this arm measures."
  fi
fi

# ---- the crash-resume run ----------------------------------------------

echo
echo "==> starting a sync and delivering the inbound event"
STARTED="$(curl -fsS --max-time 15 -X POST "$API/api/workflows/integration-hub/start" "${auth[@]}" \
  -H "Content-Type: application/json" -H "Idempotency-Key: ih-$SUFFIX" \
  -d "{\"input\":{\"source_id\":\"$SOURCE_ID\",\"webhook_id\":\"$WEBHOOK_ID\",\"customer_id\":\"cus-1\",\"event_type\":\"contact.updated\",\"payload\":{\"id\":\"c-1\"}}}" |
  python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
[[ -n "$STARTED" ]] || { echo "FAIL: the start returned no run id" >&2; exit 1; }
echo "    run ${STARTED:0:8}…"

# The run parks on the inbound event; deliver it, exactly as the customer's
# system would. This is the ROUTES hitch point doing real work: the run does not
# proceed until a signed POST arrives at the ingest endpoint.
#
# THE EVENT TYPE IS A HEADER, NOT A BODY FIELD, and that is the whole reason
# this line reads the way it does. `webhook-ingest` takes it from
# `X-Github-Event` or `X-Event-Type` and falls back to the literal string
# "webhook" when neither is present (plugins/webhookingest/routes.go:248-253).
# A body field named `event_type` is carried through as payload and ignored for
# routing.
#
# Measured: this delivered `{"event_type":"contact.updated"}` in the body and no
# header, the event was stored as `webhook`, `await_webhook` filtered on
# `contact.updated`, found nothing, and the run failed 30s later with
# "no contact.updated event from source …". The failure names the wait rather
# than the mismatch, so the ingress contract is what to check first.
BODY='{"event_type":"contact.updated","payload":{"id":"c-1"}}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac 'whsec_local_dev' -r | cut -d' ' -f1)"
if ! curl -fsS --max-time 15 -X POST "$API/ingest/$SOURCE_ID" \
    -H "Content-Type: application/json" \
    -H "X-Event-Type: contact.updated" \
    -H "X-Hub-Signature-256: $SIG" -d "$BODY" >/dev/null 2>&1; then
  echo "FAIL: the signed inbound event was refused by POST /ingest/{source_id}" >&2
  failures=$((failures + 1))
fi

# WAIT FOR THE DISPATCH, NOT FOR THE RUN. The kill has to land after the
# connector call and before the run ends, and the delivery row is how that
# moment is observed.
echo
echo "==> waiting for the connector dispatch (the delivery row)"
dispatched=0
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  if [[ "$(delivery_count)" != "0" && "$(delivery_count)" != "-1" ]]; then dispatched=1; break; fi
  sleep 1
done
if (( dispatched == 0 )); then
  echo "FAIL: no delivery row appeared within 90s, so there is nothing to crash after." >&2
  echo "      run status: $(run_status "$STARTED")" >&2
  failures=$((failures + 1))
else
  echo "    dispatch observed (rows: $(delivery_count))"

  # ---- THE KILL ----
  #
  # SIGKILL, not `stop`: a graceful stop drains the run (--shutdown-grace) and
  # would resume it cleanly, which is the case that already works. The
  # interesting one is a worker that dies without finishing, which is what a
  # crash actually is.
  echo
  echo "==> SIGKILLing the worker mid-run"
  "${COMPOSE[@]}" kill -s SIGKILL cleat-worker >/dev/null 2>&1
  echo "    killed; the run is now owned by a worker that does not exist"

  echo "==> restarting it"
  # Stamp the moment, so every later log dump can ask for lines SINCE it. The
  # old dump asked for the last 60 lines, which after a restart are the
  # PRE-KILL startup burst -- a healthy worker -- so a failure to come back was
  # reported with evidence of the worker that had just been killed.
  RESTART_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if ! "${COMPOSE[@]}" up -d cleat-worker >/tmp/ih-restart.log 2>&1; then
    echo "FAIL: could not restart the worker" >&2
    cat /tmp/ih-restart.log >&2
    failures=$((failures + 1))
  fi

  # cleat#2777: `up -d`'s own exit code is NOT proof the container is
  # running. Measured live: it returned 0 here while cleat-worker sat
  # Exited(137) from the SIGKILL above, never restarted -- compose can
  # report success while doing nothing. So ask docker directly rather than
  # trusting the command that was supposed to act. 15s is generous for a
  # container that is actually starting (no restart policy or healthcheck
  # gates cleat-worker's own "running" state in this compose file, so it
  # transitions in well under that); a container that never reaches
  # "running" at all is not going to at 16s either.
  if ! wait_for_worker_state running 15; then
    # A final determination, not a re-use of whatever wait_for_worker_state
    # last saw: it can return 1 having only ever seen "unknown" (cleat#2803
    # review), and that is a materially different report from "confirmed
    # not running" -- the fix must not turn an undetermined read into a
    # confident negative, which is the exact defect this fix exists for.
    if final_state="$(worker_container_state 2>/tmp/ih-worker-state-diag.err)"; then
      echo "FAIL: cleat-worker is NOT RUNNING after the restart." >&2
      echo "This is not a slow start -- the container never came up. Current" >&2
      echo "state: '$final_state'." >&2
    else
      echo "FAIL: could not determine cleat-worker's state after the restart:" >&2
      echo "  $(cat /tmp/ih-worker-state-diag.err 2>/dev/null)" >&2
      echo "This is NOT a confirmed 'not running' -- docker itself would not say." >&2
    fi
    echo >&2
    echo "--- what the container actually is ---" >&2
    "${COMPOSE[@]}" ps -a >&2 2>&1 || true
    echo "--- docker compose up's own output (full, not a tail) ---" >&2
    cat /tmp/ih-restart.log >&2 2>&1 || true
    echo "--- the worker's log SINCE THE RESTART (not --tail, which the" >&2
    echo "    pre-kill startup burst fills: that is what the old dump showed)" >&2
    "${COMPOSE[@]}" logs --since "${RESTART_AT:-5m}" cleat-worker >&2 2>&1 || true
    exit 1
  fi

  # THE WAIT IS GENEROUS AND THE FAILURE IS TERMINAL, and both halves are a fix
  # rather than a preference. Measured on CI (cleat#2562's run): the restart
  # exceeded a 60s budget, and because the script CONTINUED, eight later
  # assertions fired against a dead worker and named eight wrong causes --
  # `deliveries through the backend = '-1'`, `PUT /rate-limits/edge = 000` (a
  # refused connection), `no request in a burst of 40 was rate limited`. A
  # reader sees "9 assertion(s) failed" and has to deduce which one is the cause.
  #
  # So: wait long enough that a merely-slow runner is not a failure, and STOP
  # when the worker is genuinely gone, because every assertion after this point
  # is a measurement of the worker rather than of the scenario.
  #
  # Past this point the container has already been confirmed running (above),
  # so a plain healthz timeout here genuinely IS "not ready yet" -- migrations,
  # warmup -- not "not running": cleat#2777's defect was reporting the two
  # identically. If the container dies again during this wait, that is "not
  # running" again, and gets its own fail-fast below rather than waiting out
  # the rest of the 180s budget to report the wrong cause.
  deadline=$((SECONDS + 180))
  until curl -fsS --max-time 5 "$API/healthz" >/dev/null 2>&1; do
    # An UNDETERMINED read (worker_container_state returning 1 -- `ps`
    # itself failing, a transient docker CLI error) is not evidence of
    # anything and must not fail this loop: cleat#2803's review measured
    # exactly this happening about once a second across a 180s wait, and
    # the earlier version blamed the worker for a docker hiccup. Only a
    # DETERMINED non-running state below fails fast; an undetermined one
    # falls through to the deadline check like any other retry.
    if state="$(worker_container_state 2>/dev/null)" && [[ "$state" != "running" ]]; then
      echo "FAIL: cleat-worker stopped running while waiting for /healthz." >&2
      echo "This is NOT a 180s timeout -- the container exited during the wait." >&2
      echo "Current state: '$state'." >&2
      echo >&2
      echo "--- what the container actually is ---" >&2
      "${COMPOSE[@]}" ps -a >&2 2>&1 || true
      echo "--- the worker's log SINCE THE RESTART ---" >&2
      "${COMPOSE[@]}" logs --since "${RESTART_AT:-5m}" cleat-worker >&2 2>&1 || true
      exit 1
    fi
    if (( SECONDS > deadline )); then
      echo "FAIL: cleat-worker did not answer /healthz within 180s." >&2
      echo "(Last determined state: '${state:-<undetermined>}'. 'not running' would" >&2
      echo "have failed fast, above, instead of reaching here -- so either the" >&2
      echo "container has genuinely been up the whole wait and is just slow, or" >&2
      echo "docker itself was not answering right at this instant; either way this" >&2
      echo "is 'not ready yet', not a confirmed 'not running'.)" >&2
      echo >&2
      echo "THE CRASH-RESUME ASSERTION WAS NOT EVALUATED, and that is not the same" >&2
      echo "as its having failed. It sits below this point, so this run says NOTHING" >&2
      echo "about whether a resumed run dispatches twice -- and a reader told that" >&2
      echo "the crash-resume check is red would go into the resume path, where" >&2
      echo "nothing here has been measured." >&2
      echo >&2
      echo "STOPPING HERE rather than running the rest: every later assertion would" >&2
      echo "run against a dead worker and name its own subject as the cause. That is" >&2
      echo "what this script did on 2026-09-28, turning one unreturned container into" >&2
      echo "nine failures across nine subjects." >&2
      echo >&2
      echo "--- what the container actually is ---" >&2
      "${COMPOSE[@]}" ps -a >&2 2>&1 || true
      echo "--- docker's own restart output ---" >&2
      cat /tmp/ih-restart.log >&2 2>&1 || true
      echo "--- the worker's log SINCE THE RESTART (not --tail, which the" >&2
      echo "    pre-kill startup burst fills: that is what the old dump showed)" >&2
      "${COMPOSE[@]}" logs --since "${RESTART_AT:-5m}" cleat-worker >&2 2>&1 || true
      exit 1
    fi
    sleep 2
  done

  # The reaper reclaims the orphaned run after a heartbeat grace period, then
  # the settle sleep elapses. Generous, because this is the assertion that
  # matters and a timeout here would read as a defect in the workflow.
  echo
  echo "==> waiting for the resumed run to finish"
  if ! wait_for_status "$STARTED" "done" 180; then
    echo "FAIL: the resumed run did not reach done; it reads '$(run_status "$STARTED")'" >&2
    failures=$((failures + 1))
  else
    echo "    resumed and completed"

    # ---- THE ASSERTION ----
    #
    # One dispatch, one row. A second row means the engine did not record the
    # send_webhook call and the resumed run made it again -- which is the
    # duplicate delivery to the customer's CRM that the recording exists to
    # prevent.
    echo
    echo "==> the crash-resume assertion"
    check "delivery rows after crash and resume" "$(delivery_count)" "1"

    # ---- the background loop, and the permission it needs ----
    #
    # THE SINK IS PRIVATE AND THE DELIVERY LANDS ANYWAY, because the compose
    # grants exactly one host by name:
    #
    #   --plugin-egress-allow-private=sink
    #
    # WITHOUT THAT FLAG the delivery is refused, and the refusal reads like a
    # closed door rather than a permission:
    #
    #   notifications: delivery retrying attempt=1
    #   reason="request failed: Post \"http://sink:9099/hook\": egress to sink
    #   (172.19.0.3) is refused by cleat's network policy: RFC1918 private"
    #
    # That is the floor working. `engine/egress_policy.go` marks the RFC1918
    # prefixes `exemptible: true`, so an operator may name a host such as a
    # self-hosted model server -- or, here, the customer's own system across a
    # private network, which is what an integration hub is FOR.
    #
    # This assertion therefore covers the background-loop hitch point end to
    # end: the loop picks up the pending row, makes the HTTP request, and the
    # sink receives it.
    echo
    echo "==> the background loop delivers to the rope end"
    hits=0
    deadline=$((SECONDS + 90))
    while (( SECONDS < deadline )); do
      hits="$(sink_hits)"
      [[ "$hits" == "1" ]] && break
      sleep 3
    done
    if [[ "$hits" == "1" ]]; then
      echo "    ok      the sink received exactly 1 delivery"
      echo "            (a private host, permitted by --plugin-egress-allow-private)"
    else
      echo "    FAIL    the sink received '$hits' deliveries, want 1" >&2
      if "${COMPOSE[@]}" logs --tail=100 cleat-worker 2>&1 | grep -q "network policy"; then
        echo "            the worker refused the egress:" >&2
        "${COMPOSE[@]}" logs --tail=100 cleat-worker 2>&1 | grep -o 'egress to [^"]*' | tail -1 >&2
      fi
      failures=$((failures + 1))
    fi
  fi
fi

# ---- the KNOWN POSITIVE for the counter above --------------------------
#
# THE ASSERTION ABOVE REPORTED 1, AND A COUNTER THAT CAN ONLY EVER REPORT 1
# WOULD HAVE PASSED IT IDENTICALLY. That is the failure this repo's rules are
# about: a check that cannot disagree with the claim it is checking is a claim,
# not a check.
#
# So move the number on purpose. A second sync against the same connector must
# produce a SECOND delivery row -- and if it does, the "1" above was a
# measurement rather than a constant. A replayed call would look exactly like
# this: two rows for one customer's event.
#
# Note the shape: this is the same instrument, on the same subject, in a
# different state. Not a second instrument.
echo
echo "==> known positive: a second sync must move that count to 2"

SECOND="$(curl -fsS --max-time 15 -X POST "$API/api/workflows/integration-hub/start" "${auth[@]}" \
  -H "Content-Type: application/json" -H "Idempotency-Key: ih-$SUFFIX-2" \
  -d "{\"input\":{\"source_id\":\"$SOURCE_ID\",\"webhook_id\":\"$WEBHOOK_ID\",\"customer_id\":\"cus-2\",\"event_type\":\"contact.updated\",\"payload\":{\"id\":\"c-2\"}}}" |
  python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
[[ -n "$SECOND" ]] || { echo "FAIL: the second start returned no run id" >&2; failures=$((failures + 1)); }

# A second signed event, because the first run has already taken the first one.
SIG2="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac 'whsec_local_dev' -r | cut -d' ' -f1)"
if ! curl -fsS --max-time 15 -X POST "$API/ingest/$SOURCE_ID" \
    -H "Content-Type: application/json" \
    -H "X-Event-Type: contact.updated" \
    -H "X-Hub-Signature-256: $SIG2" -d "$BODY" >/dev/null 2>&1; then
  echo "FAIL: the second signed inbound event was refused" >&2
  failures=$((failures + 1))
fi

count=0
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  count="$(delivery_count)"
  [[ "$count" == "2" ]] && break
  sleep 2
done
check "delivery rows after a second sync" "$count" "2"

# ---- the backend, and the page it serves -------------------------------
#
# The backend is what the README tells a reader to run. Leaving it unexercised
# here would make it the one artefact in this example that is READ rather than
# RUN -- which is the failure this scenario exists to catch, one level up.
#
# The API key is passed in the environment and attached by the backend's
# transport; the page never holds one. That is asserted below rather than
# assumed: the page's own fetch of /api/deliveries must return rows while the
# browser-side code has no key to send.
#
# IT RUNS AFTER EVERY COUNT ABOVE, ON PURPOSE: it creates a third sync and a
# third delivery row, so the crash-resume assertion and its known positive have
# both been read by the time this section starts.
#
# AND IT RUNS BEFORE THE RATE-LIMIT SECTION, ALSO ON PURPOSE -- see that
# section's own comment. Short version: once the burst there has spent the
# plugin's bucket, EVERY request is refused for the rest of the window,
# including the ones this section makes. Measured on a run that had them in the
# other order: `GET /api/deliveries` came back 502 (this backend tunnelling the
# plugin's 429) and `POST /api/syncs` failed with "unexpected status 429", so two
# assertions reported the previous section's limit rather than anything about
# the backend.
echo
echo "==> starting the backend and its page"
WEB_PORT="$(free_port)"
(
  cd "$EXAMPLE_DIR" || exit 1
  # CLEAT_SOURCE_ID, not a per-request field: which source a deployment's events
  # arrive through is a property of the deployment, and the ingest route resolves
  # the tenant from that ROW rather than from the request (cleat#1538) -- so a
  # caller that could name any source could dispatch into somebody else's.
  CLEAT_URL="$API" CLEAT_API_KEY="$API_KEY" \
  CLEAT_SOURCE_ID="$SOURCE_ID" CLEAT_WEBHOOK_ID="$WEBHOOK_ID" \
    go run ./backend -listen "127.0.0.1:$WEB_PORT" -web ./web
) >/tmp/ih-backend.log 2>&1 &
BACKEND_PID=$!
# No second trap: the EXIT trap above already tears the stack down, and replacing
# it here would drop cleanup's exit-status capture.

WEB="http://127.0.0.1:$WEB_PORT"
deadline=$((SECONDS + 90))
until curl -fsS "$WEB/" >/dev/null 2>&1; do
  if (( SECONDS > deadline )); then
    echo "FAIL: the backend did not answer within 90s" >&2
    tail -30 /tmp/ih-backend.log >&2
    failures=$((failures + 1))
    break
  fi
  sleep 1
done

if curl -fsS "$WEB/" >/dev/null 2>&1; then
  echo "    ok      the page is served"

  # The page's two assets, which nothing else here would notice. A page whose
  # script 404s renders as a blank form: the node in index.html still arrives, so
  # a check on `/` alone says the page is served and means only that the HTML is.
  for asset in app.js app.css; do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$WEB/$asset" 2>/dev/null)"
    check "the page's $asset is served" "$code" "200"
  done

  # THE PAGE'S STATUS VOCABULARY IS CHECKED AGAINST THE CODE THAT WRITES IT,
  # because NOTHING ELSE IN THIS SCENARIO EXECUTES THE PAGE'S JAVASCRIPT. Every
  # other assertion here reads JSON the backend produced; a wrong branch in
  # app.js is invisible to all of them -- the assets are served, the numbers are
  # right, and the page is wrong.
  #
  # Measured: app.js's pillFor had `case "completed"`, and `completed` is not in
  # workflow_instances.status at all (engine/status_vocabulary.go; a finished run
  # is "done"). Every finished run would have painted as the fallback colour.
  # That was found by a scenario failure with the same misspelling in a wait
  # condition -- luck, not this check -- which is why the check now exists.
  #
  # It is a SUBSTRING test, deliberately loose: the question is whether the word
  # appears in the code that writes it, not how it is spelled there. An empty
  # extraction FAILS rather than passing, so a refactor that stops this finding
  # the case labels reports itself instead of going quiet.
  missing="$(python3 - "$EXAMPLE_DIR/web/app.js" <<'PY'
import pathlib, re, sys
page = pathlib.Path(sys.argv[1]).read_text()
cases = sorted(set(re.findall(r'case "([a-z_]+)":', page)))
if not cases:
    print("EXTRACTED-NOTHING-FROM-THE-PAGE")
    raise SystemExit
sources = " ".join(p.read_text() for p in [
    pathlib.Path("engine/status_vocabulary.go"),   # the RUN vocabulary
    pathlib.Path("plugins/notifications/background.go"),
    pathlib.Path("plugins/notifications/routes.go"),  # the DELIVERY vocabulary
])
print(",".join(c for c in cases if f'"{c}"' not in sources and f"'{c}'" not in sources))
PY
)"
  check "every status the page paints is one the code writes" "$missing" ""

  # The page's OWN route, not the worker's: this is the backendkit proxy path,
  # and the delivery log is the plugin route backendkit does not cover.
  logged="$(curl -fsS --max-time 10 "$WEB/api/deliveries" 2>/dev/null |
    python3 -c 'import json,sys
try: print(len(json.load(sys.stdin).get("deliveries") or []))
except Exception: print("-1")')"
  # check, not contains: "2" as a substring is satisfied by "12", and the point
  # of this line is that the app's delivery log sees exactly the rows the
  # scenario counted above -- the same number, through a different reader.
  check "deliveries through the backend" "$logged" "2"

  # PER-TENANT FILTERING IS AN ASSERTION, NOT A FILTER, and this is where that
  # is visible: the worker scopes the route by the API key's tenant, so a second
  # tenant is a second key and the backend says so rather than returning nothing.
  other="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
    "$WEB/api/deliveries?tenant=somebody-else" 2>/dev/null)"
  check "a second tenant is refused" "$other" "403"

  # A sync started THROUGH the app, then the inbound event delivered THROUGH the
  # app -- which signs server-side, because the secret is the source's and a
  # browser must not hold it. That is the whole page's path in two calls.
  APP_RUN="$(curl -fsS --max-time 15 -X POST "$WEB/api/syncs" \
    -H "Content-Type: application/json" -H "Idempotency-Key: ih-$SUFFIX-web" \
    -d '{"customer_id":"cus-3","event_type":"contact.updated","payload":{"id":"c-3"}}' |
    python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
  if [[ -z "$APP_RUN" ]]; then
    echo "FAIL: the backend started no run" >&2
    failures=$((failures + 1))
  else
    echo "    started ${APP_RUN:0:8}… through the backend"
    if curl -fsS --max-time 15 -X POST "$WEB/api/inbound" \
        -H "Content-Type: application/json" \
        -d '{"event_type":"contact.updated","payload":{"id":"c-3"}}' >/dev/null 2>&1; then
      echo "    ok      the backend signed and delivered the inbound event"
    else
      echo "FAIL: POST /api/inbound was refused" >&2
      failures=$((failures + 1))
    fi
    # "done", NOT "completed". `completed` is not in
    # workflow_instances.status at all -- the settled vocabulary is
    # engine/status_vocabulary.go's done | failed | dead_lettered | cancelled |
    # terminated. Measured: with "completed" here the wait ran its full 180s
    # against a run that had already finished and then reported
    # "FAIL: the app's run did not complete; it reads 'done'", which is the
    # failure message and the passing status in one line.
    if wait_for_web_status "$APP_RUN" "done" 180; then
      echo "    ok      the run completed end to end through the app"
    else
      echo "FAIL: the app's run did not complete; it reads '$(web_status "$APP_RUN")'" >&2
      failures=$((failures + 1))
    fi
  fi
fi

# `disown` BEFORE the kill, so the shell does not print its own job-control
# notice -- "Terminated: 15 ( cd ... go run ./backend ... )" -- into a CI log
# where it reads exactly like a failure, on a run that is about to report that
# all assertions passed. The kill itself is unchanged, and `go run` forwards the
# signal to the binary it started.
disown "$BACKEND_PID" 2>/dev/null || true
kill "$BACKEND_PID" 2>/dev/null || true

# ---- the wedge: the tenant's own step, not yours -----------------------
#
# docs/playbooks/integration-hub.md, "The wedge": a customer uploads its own
# transform through POST /api/definitions, and SyncCustomer (hub.go) invokes
# it as a child workflow. Everything above this point deploys and runs the
# OPERATOR's own workflow; nothing above ever calls POST /api/definitions at
# all, so the tenant-uploaded-step claim was undemonstrated. This is that
# claim, executed against the same real deployed worker.
#
# RUNS HERE, before the rate-limit burst spends the bucket below and after
# the backend section's own counted delivery, so this section's own dispatch
# does not disturb an absolute delivery count any earlier assertion checks.
echo
echo "==> the wedge: uploading and running the tenant's own step"

TENANT_WASM_DIR="$(mktemp -d)"
if ! "$CLEAT_BIN" build --target go -o "$TENANT_WASM_DIR" \
    ./examples/integration-hub/tenant-steps/normalize-order/ >/tmp/ih-tenant-build.log 2>&1; then
  echo "FAIL: building the tenant's own step (normalize-order) failed" >&2
  tail -25 /tmp/ih-tenant-build.log >&2
  failures=$((failures + 1))
else
  TENANT_WASM="$TENANT_WASM_DIR/normalize-order.wasm"
  if [[ ! -f "$TENANT_WASM" ]]; then
    echo "FAIL: the tenant step build did not produce $TENANT_WASM" >&2
    ls -la "$TENANT_WASM_DIR" >&2
    failures=$((failures + 1))
  else
    # The base64 payload goes over STDIN, not argv: a multi-megabyte string
    # as a single shell/exec argument risks E2BIG ("Argument list too
    # long") on some systems -- measured hitting it here with a ~5 MB wasm.
    DEF_JSON_FILE="$OUT_DIR/normalize-order-def.json"
    base64 < "$TENANT_WASM" | tr -d '\n' | python3 -c '
import json, sys
print(json.dumps({"name": "normalize-order", "wasm_bytes_base64": sys.stdin.read()}))
' >"$DEF_JSON_FILE"
    # --data @file, NOT -d "$(cat file)": the JSON body is several MB once the
    # wasm is base64-encoded, and passing it as a single shell/exec argument
    # risks E2BIG ("Argument list too long") -- measured hitting it here.
    if ! curl -fsS --max-time 30 -X POST "$API/api/definitions" "${auth[@]}" \
        -H "Content-Type: application/json" --data @"$DEF_JSON_FILE" >/tmp/ih-tenant-upload.log 2>&1; then
      echo "FAIL: POST /api/definitions (the tenant's own upload) was refused" >&2
      cat /tmp/ih-tenant-upload.log >&2
      failures=$((failures + 1))
    else
      echo "    ok      the tenant's own step was uploaded through POST /api/definitions"

      TENANT_RUN="$(curl -fsS --max-time 15 -X POST "$API/api/workflows/integration-hub/start" "${auth[@]}" \
        -H "Content-Type: application/json" -H "Idempotency-Key: ih-$SUFFIX-wedge" \
        -d "{\"input\":{\"source_id\":\"$SOURCE_ID\",\"webhook_id\":\"$WEBHOOK_ID\",\"customer_id\":\"cus-wedge\",\"event_type\":\"contact.updated\",\"payload\":{\"id\":\"c-wedge\"},\"tenant_step_name\":\"normalize-order\"}}" |
        python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
      if [[ -z "$TENANT_RUN" ]]; then
        echo "FAIL: the wedge sync returned no run id" >&2
        failures=$((failures + 1))
      else
        # normalize-order requires order_id (examples/integration-hub/tenant-steps/normalize-order/main.go)
        # -- $BODY above has no such field, so the wedge needs its own inbound
        # payload rather than reusing the backend section's.
        #
        # NO {"event_type":...,"payload":{...}} WRAPPER, unlike $BODY above:
        # plugins/webhookingest/routes.go's webhookPayload(body) forwards the
        # RAW POST BODY verbatim as inbound.Payload -- it does not unwrap a
        # nested "payload" key -- and eventType is read from the X-Event-Type
        # header below, not from the body. A wrapped body here would make
        # order_id live at .payload.order_id instead of the top level
        # normalize-order's json.Unmarshal expects (measured: "order_id is
        # required" with the wrapper in place).
        WEDGE_BODY='{"order_id":"ord-wedge-1","vendor_name":"acme"}'
        SIGW="sha256=$(printf '%s' "$WEDGE_BODY" | openssl dgst -sha256 -hmac 'whsec_local_dev' -r | cut -d' ' -f1)"
        if ! curl -fsS --max-time 15 -X POST "$API/ingest/$SOURCE_ID" \
            -H "Content-Type: application/json" \
            -H "X-Event-Type: contact.updated" \
            -H "X-Hub-Signature-256: $SIGW" -d "$WEDGE_BODY" >/dev/null 2>&1; then
          echo "FAIL: the wedge's signed inbound event was refused" >&2
          failures=$((failures + 1))
        fi

        if wait_for_status "$TENANT_RUN" "done" 60; then
          RESULT_JSON="$(curl -fsS --max-time 10 "$API/api/workflows/$TENANT_RUN" "${auth[@]}" 2>/dev/null |
            python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("result",""))
except Exception: print("")')"
          TENANT_STEP_RAN="$(printf '%s' "$RESULT_JSON" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("tenant_step_ran", False))
except Exception: print(False)')"
          check "tenant_step_ran" "$TENANT_STEP_RAN" "True"
        else
          echo "FAIL: the wedge run did not complete; it reads '$(run_status "$TENANT_RUN")'" >&2
          failures=$((failures + 1))
        fi
      fi
    fi
  fi
  rm -rf "$TENANT_WASM_DIR"
fi

# ---- the edge middleware -----------------------------------------------
#
# THIS SECTION RUNS LAST, AND THE ORDER IS LOAD-BEARING. The burst below spends
# the plugin's bucket -- 5 per 60s -- and `ratelimiter`'s middleware wraps EVERY
# request for a tenant that has any bucket configured, including the plugin's own
# management routes. So for the rest of the window:
#
#   * every request from the backend section is refused, which is what made two
#     of its assertions fail when the two sections were in the other order; and
#   * `PUT /rate-limits/edge` is refused too, so the bucket cannot be given back
#     from inside the scenario -- measured: a restore to max_requests=100000
#     answered 429, as did a plain GET /api/workflows.
#
# That second one is a property of the plugin rather than of this script, and it
# is filed as cleat#2551. Ordering the sections is the honest response here: this
# scenario is exercising four hitch points, not testing what a tenant does when
# it has exhausted its own limit.

echo
echo "==> the rate limit (edge middleware)"
#
# THE HITCH POINT IS THE PLUGIN'S MIDDLEWARE, AND IT NEEDS A LIMIT SEEDED.
#
# There are two rate limiters and they are different mechanisms:
#
#   CORE     `--rate-limit` (per IP, default 100/s burst 200) and
#            `--rate-limit-per-tenant` (per tenant, default 0 = off).
#            Applied by `rateLimitMiddleware`, and its 429 sets NO
#            X-RateLimit-* headers (cmd/cleat-worker/server.go, write429).
#
#   PLUGIN   `ratelimiter`, the edge middle. Its token buckets are built from
#            ITS OWN TABLE, one per (tenant, limit key), refreshed by its
#            background loop -- so it does nothing until a limit exists.
#            Its 429 DOES set X-RateLimit-Limit and X-RateLimit-Remaining
#            (plugins/ratelimiter/middleware.go).
#
# Both answer 429 with the body {"error":"rate limit exceeded"}, so the BODY
# cannot tell them apart -- an earlier version of this script matched on it and
# could not have discriminated. THE HEADER IS THE DISCRIMINATOR.
#
# The compose's `--rate-limit-per-tenant` is the core's, not this plugin's: an
# earlier version of the compose comment said otherwise and would have made a
# reader think the edge middleware was configured when it was not.
echo
echo "==> seeding a per-tenant limit on the plugin"
KEYSET="$(curl -fsS --max-time 10 -X PUT "$API/rate-limits/edge" "${auth[@]}" \
  -H "Content-Type: application/json" \
  -d '{"max_requests":5,"window_seconds":60}' -w '%{http_code}' -o /dev/null 2>/dev/null)"
if [[ "$KEYSET" =~ ^2 ]]; then
  echo "    ok      PUT /rate-limits/edge = $KEYSET (5 requests per 60s)"
else
  echo "    FAIL    PUT /rate-limits/edge = $KEYSET, so the plugin has no limit to enforce" >&2
  failures=$((failures + 1))
fi

# A burst that clears the plugin's limit of 5 without approaching the core's
# per-IP burst of 200, so a 429 here can only have come from the plugin.
echo
echo "==> bursting 40 authenticated requests"
# codes is an ARRAY, and that is not a style preference. It was a
# space-separated string whose ONE consumer relied on word splitting --
# `printf '%s\n' $codes` -- to put each code on its own line, so the correct
# repair is not to quote it (that prints one line and reports 0 of 40) but to
# stop encoding a list as a string. ShellCheck flags the unquoted expansion as
# SC2086 and shellcheck is right; the old form was load-bearing only because a
# list was being spelled as text.
codes=(); with_header=0
for _ in $(seq 1 40); do
  out="$(curl -s -D - --max-time 5 "${auth[@]}" -o /dev/null "$API/api/workflows" 2>/dev/null)"
  code="$(printf '%s' "$out" | head -1 | awk '{print $2}')"
  codes+=("$code")
  [[ "$code" == "429" ]] && printf '%s' "$out" | grep -qi '^X-RateLimit-Limit:' && with_header=$((with_header + 1))
done
over=$(printf '%s\n' "${codes[@]}" | grep -c '^429$')

if (( over == 0 )); then
  echo "    FAIL    no request in a burst of 40 was rate limited" >&2
  echo "            (codes seen: $(printf '%s\n' "${codes[@]}" | sort -u | tr '\n' ' '))" >&2
  failures=$((failures + 1))
else
  echo "    ok      $over of 40 requests were rate limited (429)"
  if (( with_header > 0 )); then
    echo "    ok      $with_header carried X-RateLimit-Limit: the PLUGIN's edge"
    echo "            middleware fired on the limit seeded above"
  else
    echo "    FAIL    every 429 came from the core limiter, not the plugin's edge" >&2
    echo "            middleware: none carried X-RateLimit-Limit, so the hitch" >&2
    echo "            point this scenario claims to exercise did not run" >&2
    failures=$((failures + 1))
  fi
fi

# ---- report ------------------------------------------------------------

echo
echo "documented commands run: $ran of $total"

if (( ran < total )); then
  echo "UNMEASURED: ran $ran of $total documented commands; a clean result would mean nothing." >&2
  exit 2
fi

if (( failures > 0 )); then
  echo >&2
  echo "$failures assertion(s) failed. The scenario does not behave as its README says." >&2
  exit 1
fi

echo "all assertions passed"
