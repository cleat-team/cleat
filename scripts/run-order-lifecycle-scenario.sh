#!/usr/bin/env bash
#
# Run examples/order-lifecycle end to end against a real worker and a real
# PostgreSQL, and assert what each run PUBLISHED. cleat#2512.
#
# WHY THIS EXISTS
#
# `scripts/build-documented-examples.sh` builds ten examples and runs none of
# them. Measured 2026-09-17 in that script's own header: 18 example directories,
# 10 documenting a `cleat build`, ZERO built by any workflow. Building is now
# covered. Running is not, and running is where the interesting failures are: an
# example whose workflow compiles, deploys, and then cannot reach its plugin
# fails in a way no build check can see.
#
# So this is the first example in the tree that is executed rather than only
# read. Its assertions are about the SAGA, because that is the thing an example
# named order-lifecycle exists to demonstrate:
#
#   1. a declined charge unwinds the step that completed, and NOT one that did
#      not -- the distinction a hand-written rollback gets wrong;
#   2. a successful order reaches `done` only after a real webhook is delivered
#      to the bundled webhook-ingest plugin;
#   3. a compensation that RUNS AND FAILS is reported separately from one that
#      ran and succeeded, because an order whose refund failed is in the state
#      the saga exists to avoid and must not read as compensated.
#
# DERIVED, NOT LISTED
#
# The two `cleat` commands come out of the example's README, verbatim, the way
# build-documented-examples.sh takes its `cleat build`. A retyped command is a
# second source of truth, and the day the README changes is the day this job
# tests the old thing.
#
# WHAT IT SUBSTITUTES, AND WHY THAT IS NOT A LIE ABOUT THE README
#
# Three substitutions, all of them environment rather than content:
#
#   - the worker image. The compose file's default is published by a release, so
#     a PR cannot pull the one that reflects its own change. The DEFAULT is
#     asserted separately below, so renaming it in the compose file is still
#     caught.
#   - the two host ports, so a runner with something already on 5432 or 8080
#     does not fail a job about the example.
#   - `cleat` itself, so the freshly built binary is used rather than whatever
#     happens to be on PATH.
#
# Every substitution is a variable the example already documents. The commands'
# own text -- the flags, the workflow name, the artifact path -- is the README's.
#
# DIALECT ARMS -- cleat#2560
#
#   scripts/run-order-lifecycle-scenario.sh [postgres|mysql|mssql]
#
# The compose file carries three databases and a profile per dialect, so this
# takes one and drives that arm end to end. Default postgres.
#
# WHAT THE ARM DOES AND DOES NOT CLAIM, because the whole value of a dialect
# matrix is that the per-dialect differences are stated rather than averaged:
#
#   - **The saga assertions are dialect-independent and are asserted
#     IDENTICALLY on all three.** Every check below reads either the run's
#     `error` field or its published query state. None of them is relaxed for a
#     dialect, and none may be: an arm that passes because it asserts less is
#     the failure this exists to avoid. If one of these turns out not to hold on
#     MySQL, that is a finding about MySQL and not a reason to drop the check.
#   - **There is exactly one place the dialects genuinely differ, and it is
#     asserted differently on purpose.** PostgreSQL enforces row-level security
#     and the worker verifies its runtime connection is subject to it; MySQL and
#     SQL Server have no such check at all, because
#     `cmd/cleat-worker/main.go` gates the whole thing on `--driver ==
#     "postgres"` before calling `engine.CheckRLSEnforced` -- a function that
#     queries `pg_roles` and could not run elsewhere. So this script asserts the
#     guarantee where it exists and STATES ITS ABSENCE where it does not, rather
#     than asserting the weaker thing and calling it the same assertion. See
#     assert_rls_posture below.
#
# The README's dialect table is the reader-facing half of the same statement.
set -uo pipefail

# ERREXIT OFF, EXPLICITLY -- as a DECLARATION, not as a fix for anything.
#
# Several steps here read a non-zero exit as DATA rather than as failure:
# `state_of` pipes a `curl -f` into python, so a 404 makes the whole pipeline
# non-zero under the `pipefail` above, and the wait helpers loop until a
# condition holds rather than asserting it on the first try. This line says the
# script is not written for errexit rather than leaving that to the invoker.
#
# IT IS CURRENTLY A NO-OP UNDER CI, AND AN EARLIER VERSION OF THIS COMMENT SAID
# THE OPPOSITE. The claim was that GitHub Actions runs a `run:` step as
# `bash -e <file>` so errexit is inherited and this line is load-bearing. The
# first half is right and the second does not follow: **a child bash does not
# inherit errexit.** A `run:` block becomes a temp script run under `bash -e`,
# and this file is exec'd from it as a separate process, so it starts with
# errexit off exactly as it does from a terminal.
#
#   $ bash -e parent.sh      # parent $- = ehB   SHELLOPTS=…:errexit:…
#     child $- = hB          # SHELLOPTS=braceexpand:hashall:interactive-comments
#     child CONTINUED past false                      # <- not inherited
#
# (Sourcing DOES confer it; so does the flag on the child's own command line,
# `bash -e <this file>`. Neither is how CI invokes this script.)
#
# So the line is here for the two cases that would bite -- a reader running
# `bash -e scripts/run-order-lifecycle-scenario.sh`, and anyone sourcing it --
# and to state the intent where the intent is otherwise invisible. Stating it is
# cheap; depending on the invoker's shell is the thing worth avoiding.
set +e

cd "$(git rev-parse --show-toplevel)" || exit 2

# ---- the dialect -------------------------------------------------------

DIALECT="${1:-postgres}"
# Every per-dialect fact this script needs, in one place, so that adding a
# fourth dialect is a column rather than a search.
#
# The DSNs are the column worth reading. PostgreSQL and SQL Server carry a
# scheme and MySQL does not -- `user:pw@tcp(host:port)/db` -- so there is no one
# template with the host swapped out: the SHAPE differs, not just the host. That
# is also why the compose file states the defaults rather than deriving them.
#
# Two views of each database are needed and neither is derivable from the other:
# HOST_* is what the reader's `cleat deploy` reaches, and NET_* is what the
# worker reaches over the compose network. A host runs on `localhost`, the
# worker runs beside the service.
case "$DIALECT" in
  postgres)
    PORT_ENV=CLEAT_PG_PORT
    # The superuser, because deploy writes a workflow definition and the tables
    # holding it have row-level security FORCEd.
    HOST_DB_URL_TMPL='postgres://cleat:cleat@localhost:__PORT__/cleat?sslmode=disable'
    # The worker is the non-superuser the app-role step exists to provision.
    NET_MIGRATE_DB_URL='postgres://cleat:cleat@postgres:5432/cleat?sslmode=disable'
    NET_WORKER_DB_URL='postgres://cleat_app:cleat-app-local-dev@postgres:5432/cleat?sslmode=disable'
    ;;
  mysql)
    PORT_ENV=CLEAT_MYSQL_PORT
    HOST_DB_URL_TMPL='root:cleat@tcp(localhost:__PORT__)/cleat?tls=false&parseTime=true'
    # No app-role step and no lesser identity: cleat's MySQL is single-tenant by
    # construction (cleat#2534, D1), so there is no second role to connect as
    # and the migration and the runtime use the same credentials.
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
    # An unrecognised argument is a broken check, not a finding about the
    # example, and it must not fall through to the postgres arm -- which would
    # report a clean PostgreSQL run under the name of a dialect nobody ran.
    echo "UNMEASURED: unknown dialect '$DIALECT'." >&2
    echo "Expected one of: postgres, mysql, mssql. Nothing was run." >&2
    exit 2
    ;;
esac

EXAMPLE_DIR="examples/order-lifecycle"
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

# ---- extract the documented commands ----------------------------------
#
# Join line-continuations BEFORE looking for anything. A command written across
# two lines with a trailing backslash is one command, and a line-oriented scan
# reads the second half as a separate -- and invalid -- invocation. CLAUDE.md
# records this being gotten wrong on a workflow file whose step it was protecting
# was itself line-continued.
join_continuations() {
  awk '{ while (sub(/\\$/, "")) { if ((getline nxt) > 0) $0 = $0 nxt; else break } print }' "$1"
}

strip_comments() {
  grep -vE '^[[:space:]]*#'
}

# The README as one logical command per line, with prompt markers removed.
DOC_COMMANDS="$(join_continuations "$README" | strip_comments | sed 's/^[[:space:]]*\$ //')"

extract_one() {
  local pattern="$1"
  printf '%s\n' "$DOC_COMMANDS" | grep -m1 -E "$pattern" || true
}

BUILD_CMD="$(extract_one '(^| )cleat build ')"
DEPLOY_CMD="$(extract_one '(^| )cleat deploy ')"
DEPLOY_WF_CMD="$(extract_one 'deploy-workflow --driver')"

# The deploy step is TWO documented commands, one per group of dialects, and an
# arm needs the one for its own group. So the required set is dialect-dependent
# and is stated as such, rather than loosened to "at least one of these" -- which
# would pass an arm that had quietly stopped finding its own command.
#
# Why there are two at all: the `cleat` CLI talks to PostgreSQL only. It refuses
# a MySQL or SQL Server DSN and names `deploy-workflow` as the alternative, and
# this script asserts that refusal further down. See the README, "The deploy step
# is the one command that differs by dialect".
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
WEB_PORT="$(free_port)"
WORKER_IMAGE="cleat-order-lifecycle-test:$SUFFIX"
COMPOSE=(docker compose --profile "$DIALECT" -f "$EXAMPLE_DIR/docker-compose.yml")
OUT_DIR="$(mktemp -d)"
# The port is chosen at runtime, so the host DSN is templated rather than
# literal. CLEAT_DB_URL is the variable the README's dialect table tells a
# reader to set, so the documented command works unchanged on every dialect --
# which is the claim this arm makes about it.
HOST_DB_URL="${HOST_DB_URL_TMPL//__PORT__/$DB_PORT}"
export "$PORT_ENV=$DB_PORT" CLEAT_API_PORT="$API_PORT"
export CLEAT_DIALECT="$DIALECT"
export CLEAT_DB_URL="$HOST_DB_URL"
export CLEAT_MIGRATE_DB_URL="$NET_MIGRATE_DB_URL" CLEAT_WORKER_DB_URL="$NET_WORKER_DB_URL"
export COMPOSE_PROJECT_NAME="cleat-order-lifecycle-$SUFFIX"

# The failure counter, initialised ONCE, here, above every check that can
# increment it.
#
# It used to be reset at the head of the assertions section. That was harmless
# while nothing above it incremented the counter, and became a silent eraser the
# moment a check was added above it: the RLS-posture failure was counted and then
# zeroed, and the run reported clean. Moving it up rather than re-running the
# ordering argument each time a check is added is the point -- from here no check
# can precede the initialisation, whatever order the file is later edited into.
failures=0

# Where the non-PostgreSQL deploy binary is built. The README's second deploy
# command names this exact path (`/tmp/out/deploy-workflow`), so building it here
# is what lets the extracted command run verbatim rather than being rewritten.
# Unused on the postgres arm, where the CLI does the deploying.
DEPLOY_WF_BIN="$OUT_DIR/deploy-workflow"

# On failure the worker's own log is the only artefact that explains what
# happened, and the stack is about to be removed -- so it has to be captured
# HERE. A separate CI step for this would find nothing: by the time it runs, the
# containers are gone. (That is a check wired to nothing, which is the shape
# this repo has a whole section about.)
cleanup() {
  local rc=$?
  if (( rc != 0 )); then
    echo >&2
    echo "--- cleat-worker (tail) ---" >&2
    "${COMPOSE[@]}" logs --tail=60 cleat-worker >&2 2>&1 || true
    if [[ -n "${BACKEND_PID:-}" ]]; then
      echo "--- backend (tail) ---" >&2
      tail -30 /tmp/ol-backend.log >&2 2>&1 || true
    fi
  fi
  kill "${BACKEND_PID:-}" >/dev/null 2>&1 || true
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  docker rmi -f "$WORKER_IMAGE" >/dev/null 2>&1 || true
  rm -rf "$OUT_DIR"
}
trap cleanup EXIT

# The compose file's DEFAULT image is what a reader pulls. Substituting it here
# would hide a rename, so assert it is still the published one.
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
  if ! go build -o "$DEPLOY_WF_BIN" ./cmd/deploy-workflow >/tmp/ol-dw-build.log 2>&1; then
    echo "FAIL: the README's documented build of deploy-workflow failed:" >&2
    tail -20 /tmp/ol-dw-build.log >&2
    exit 1
  fi
fi

echo "==> building the worker image from this checkout"
if ! docker build -t "$WORKER_IMAGE" . >/tmp/ol-docker-build.log 2>&1; then
  echo "FAIL: docker build of the repository's Dockerfile" >&2
  tail -25 /tmp/ol-docker-build.log >&2
  exit 1
fi

echo "==> docker compose up"
if ! "${COMPOSE[@]}" up -d >/tmp/ol-compose-up.log 2>&1; then
  echo "FAIL: docker compose up" >&2
  tail -30 /tmp/ol-compose-up.log >&2
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

# The key is printed once, on first start, when no keys exist.
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

# ---- what THIS dialect does and does not guarantee ----------------------
#
# Announced here so the dialect is on screen before any output a reader might
# mistake for the default arm's. The CHECK itself is in the assertions section
# beside `contains`, because a bash function must be defined before it is called
# and every helper in this file is defined below this point.
echo
echo "==> dialect: $DIALECT"

# ---- the documented commands -------------------------------------------

ran=0
# Derived from the dialect's own required set above, not hardcoded: the two arms
# run two documented commands each, but they are not the same two.
total=${#REQUIRED_CMDS[@]}

run_documented() {
  local label="$1" cmd="$2"
  echo
  echo "==> ($label) \$ $cmd"
  # The extracted text must still be the subcommand it was extracted as. If the
  # README is rewritten so this line is no longer a `cleat` invocation, running
  # it would measure something else -- say so rather than running it.
  case "$cmd" in
    *"$CLEAT_BIN"*|*"$DEPLOY_WF_BIN"*) ;;
    *) echo "FAIL: the extracted $label command runs neither $CLEAT_BIN nor $DEPLOY_WF_BIN: $cmd" >&2; exit 1 ;;
  esac
  if ! eval "$cmd" >/tmp/ol-cmd.log 2>&1; then
    echo "FAIL: the README's documented command failed:" >&2
    tail -25 /tmp/ol-cmd.log >&2
    exit 1
  fi
  tail -3 /tmp/ol-cmd.log
  ran=$((ran + 1))
}

# `-o /tmp/out` is the convention every other example README uses; redirect it
# to a directory this run owns.
BUILD_RUN="${BUILD_CMD//cleat /$CLEAT_BIN }"
BUILD_RUN="${BUILD_RUN//-o \/tmp\/out/-o $OUT_DIR}"
run_documented build "$BUILD_RUN"

WASM="$OUT_DIR/order-lifecycle.wasm"
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
# second.
#
# MEASURED, 2026-09-28, and it is the whole reason this branch exists: the first
# version of this arm substituted only the DSN, on the reasoning that the engine
# and the worker both speak three dialects. Both non-PostgreSQL arms failed here,
# at this step, on the README's own command -- so what was wrong was the README's
# claim, not the arm's machinery.
if [[ "$DIALECT" == "postgres" ]]; then
  DEPLOY_RUN="${DEPLOY_CMD//cleat /$CLEAT_BIN }"
else
  DEPLOY_RUN="$DEPLOY_WF_CMD"
fi
# `/tmp/out` is the convention every other example README uses; redirect it to a
# directory this run owns. On the non-PostgreSQL arms this also rewrites
# `/tmp/out/deploy-workflow` to the binary built above, which is why that binary
# is built at exactly this path.
#
# There was a third substitution here until the dialect arm, rewriting
# `localhost:5432` to the free port. It is gone because the README no longer
# names a port: both deploy forms read `--db "$CLEAT_DB_URL"`, and CLEAT_DB_URL
# above is already the right DSN for this dialect with the right port in it.
DEPLOY_RUN="${DEPLOY_RUN//\/tmp\/out/$OUT_DIR}"
run_documented "$DEPLOY_LABEL" "$DEPLOY_RUN"

# And the refusal itself, asserted rather than routed around.
#
# The CLI's PostgreSQL-only-ness is the fact that explains why there are two
# deploy commands, so it is checked here rather than described in a comment. This
# is also what makes the limitation VISIBLE if it is ever lifted: the day `cleat
# deploy` accepts a MySQL DSN, this goes red and says the README is stale, rather
# than the arm quietly continuing to route around a limit that no longer exists.
if [[ "$DIALECT" != "postgres" ]]; then
  refuse_log="/tmp/ol-cli-refuse-$DIALECT.log"
  if "$CLEAT_BIN" deploy --db "$CLEAT_DB_URL" --name order-lifecycle "$WASM" \
       >"$refuse_log" 2>&1; then
    echo "    FAIL    cleat deploy ACCEPTED a $DIALECT DSN. This arm deploys with" >&2
    echo "            deploy-workflow because the README says the CLI is PostgreSQL-only." >&2
    echo "            One of the two is now wrong, and this arm's premise is the CLI one." >&2
    failures=$((failures + 1))
  elif grep -q 'only supports PostgreSQL' "$refuse_log"; then
    echo "    ok      cleat deploy refuses a $DIALECT DSN, naming PostgreSQL as the reason"
  else
    # Non-zero for some OTHER reason is not the documented behaviour, and must
    # not read as a pass. MEASURED 2026-09-28: `cleat deploy` exits 1 on a MySQL
    # OR SQL Server DSN (the dialect refusal) AND exits 1 on a PostgreSQL DSN
    # pointed at a closed port (a connection error). **Exit status cannot
    # separate them**, so a check on the status alone would pass this arm while
    # the README's stated reason went unverified.
    #
    # THE PHRASE IS CHOSEN CAREFULLY, AND NOT BECAUSE THE DIALECT NAME IS ABSENT
    # -- it is present. Measured on the built binary, 2026-09-28: the SQL Server
    # refusal contains `mssql` exactly once, in the REMEDY clause
    # (`deploy-workflow --driver mssql`), while the DIAGNOSIS clause reads
    # "looks like SQL Server".
    #
    # So `grep -q "$DIALECT"` WOULD match, and that is worse than a miss rather
    # than better. This check exists to assert that the CLI named the right
    # dialect as ITS REASON for refusing. The flag spelling in the remedy makes
    # no claim about the diagnosis -- it would still be there if the leading
    # clause named the wrong dialect, or named none at all. **A pass satisfied
    # by the remedy is a green tick for the wrong reason**, which is the failure
    # this arm exists to catch.
    #
    # `only supports PostgreSQL` sits in the diagnosis clause, which is the part
    # that carries the claim.
    echo "    FAIL    cleat deploy failed on $DIALECT, but not for the documented reason:" >&2
    sed 's/^/            /' "$refuse_log" >&2
    failures=$((failures + 1))
  fi
fi

# ---- the webhook source the workflow waits on --------------------------
#
# Not a documented `cleat` command -- it is two HTTP calls the README shows.
# Done here without incrementing the count, and named so that a reader can see
# it is setup rather than one of the counted steps.

echo
echo "==> creating a webhook source (setup, not a counted step)"
SOURCE_JSON="$(curl -fsS --max-time 15 -X POST "$API/ingest/sources" "${auth[@]}" \
  -H "Content-Type: application/json" \
  -d '{"name":"psp","source_type":"payment","secret":"whsec_local_dev"}' 2>&1)" || {
    echo "FAIL: POST /ingest/sources: $SOURCE_JSON" >&2
    exit 1
  }
SOURCE_ID="$(printf '%s' "$SOURCE_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
if [[ -z "$SOURCE_ID" ]]; then
  echo "FAIL: the source-create response carried no id: $SOURCE_JSON" >&2
  exit 1
fi
echo "    source ${SOURCE_ID:0:8}…"

# ---- assertions --------------------------------------------------------
#
# The counter is initialised further up, beside the first thing that can
# increment it -- the dialect's RLS posture below. It used to be reset here,
# which was harmless while this was the first check in the file and became a
# silent eraser the moment a check was added above it: the RLS failure would
# have been counted and then zeroed, and the run would have reported clean.

# state_of <run-id> <key> -> the published query value, or empty.
state_of() {
  local id="$1" key="$2"
  curl -fsS --max-time 10 "$API/api/workflows/$id/query?key=$key" "${auth[@]}" 2>/dev/null |
    python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("value",""))
except Exception: print("")'
}

# start_run <json-input> -> run id on stdout
start_run() {
  local input="$1" body
  body="$(python3 -c 'import json,sys;print(json.dumps({"input":json.loads(sys.argv[1])}))' "$input")"
  curl -fsS --max-time 15 -X POST "$API/api/workflows/order-lifecycle/start" "${auth[@]}" \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: ol-$SUFFIX-$RANDOM-$RANDOM" \
    -d "$body" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))'
}

check() {
  local what="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then
    echo "    ok      $what = $got"
  else
    echo "    FAIL    $what = '$got', want '$want'" >&2
    failures=$((failures + 1))
  fi
}

echo
# run_status / run_error read the WORKER's own record of a run. The engine's
# lifecycle status is the thing to wait on; the workflow's published state is
# not, for the reason the helpers below record.
run_status() {
  curl -fsS --max-time 10 "$API/api/workflows/$1" "${auth[@]}" 2>/dev/null |
    python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("status",""))
except Exception: print("")'
}

run_field() {
  curl -fsS --max-time 10 "$API/api/workflows/$1" "${auth[@]}" 2>/dev/null |
    python3 -c 'import json,sys
try: print(json.load(sys.stdin).get(sys.argv[1],""))
except Exception: print("")' "$2"
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

contains() {
  local what="$1" haystack="$2" needle="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "    ok      $what contains '$needle'"
  else
    echo "    FAIL    $what does not contain '$needle':" >&2
    echo "            got: $haystack" >&2
    failures=$((failures + 1))
  fi
}

# ---- what THIS dialect does and does not guarantee ----------------------
#
# The one place the three arms are deliberately not the same assertion. Read the
# DIALECT ARMS note in the header before changing it.
#
# `contains` is reused for the PostgreSQL half because the claim there is a
# positive fact the worker prints. The other half has no fact to assert, and
# saying so -- without counting it as a pass -- is the point.
#
# `failures` is initialised near the top of the file with the rest of the
# script's state, not here. Read the note there before moving it down: a counter
# reset below a check silently erases that check's failure.
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
    echo "            here. The saga assertions below are NOT weaker for it -- they are"
    echo "            the same assertions, and they are what this arm measures."
  fi
fi

# WHY THE FAILED RUNS ARE ASSERTED ON THE ERROR AND NOT ON QUERY STATE.
#
# The workflow publishes `compensated` and `unwind_failed` as query state, and
# for a run that ENDS `failed` that state is discarded before it reaches the
# database -- measured: query_state reads {} on the instance row, and the row's
# error reads "order ol-1 not completed (unwound: [reserve_inventory]; could not
# unwind: [])".
#
# `writeTerminalFailure` passes nil for the query-state argument
# (cmd/cleat-worker/setup.go) where the success path passes the harvested map
# to FinalizeWorkflowSegment. So the run's `error` is the surface that survives,
# which is why this workflow puts the trail there as well as in the state.
#
# The assertions below therefore check the trail where it is readable, and the
# run-2 case checks the published state, where it works.

echo
echo "==> run 1: the charge is declined, so the reservation unwinds"
RUN1="$(start_run "{\"order_id\":\"ol-1\",\"email\":\"buyer@example.com\",\"source_id\":\"$SOURCE_ID\",\"items\":[{\"sku\":\"widget\",\"quantity\":1,\"price_cents\":2500}],\"simulate_payment_failure\":true}")"
if ! wait_for_status "$RUN1" failed 90; then
  echo "FAIL: run 1 did not reach failed; it reads '$(run_status "$RUN1")'" >&2
  failures=$((failures + 1))
else
  # The reservation completed, so it unwinds. The charge FAILED, so it does not
  # -- and the difference between those two is the whole of what a saga gets
  # right and a hand-written rollback gets wrong.
  err1="$(run_field "$RUN1" error)"
  contains "run 1's error" "$err1" "unwound: [reserve_inventory]"
  contains "run 1's error" "$err1" "could not unwind: []"
  # The failed step is not compensated, so it must not be named as unwound.
  if [[ "$err1" == *"unwound: [reserve_inventory charge_psp]"* ]]; then
    echo "    FAIL    run 1 unwound charge_psp, whose forward never completed" >&2
    failures=$((failures + 1))
  else
    echo "    ok      run 1 did not unwind the step that failed"
  fi
fi

echo
echo "==> run 2: a webhook is delivered, and the order completes"
RUN2="$(start_run "{\"order_id\":\"ol-2\",\"email\":\"buyer@example.com\",\"source_id\":\"$SOURCE_ID\",\"items\":[{\"sku\":\"widget\",\"quantity\":1,\"price_cents\":2500}]}")"
# The run parks on the webhook for up to 30s of durable sleeps, so deliver it.
# This is the only place in the repo where the wait loop's found:false path is
# exercised at all: cleattest stubs a plugin call to ONE result, with no
# sequencing, so a poll that answers "not yet" and later "yes" cannot be
# expressed in a unit test.
BODY='{"event_type":"payment.succeeded","order_id":"ol-2"}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac 'whsec_local_dev' -r | cut -d' ' -f1)"
if ! curl -fsS --max-time 10 -X POST "$API/ingest/$SOURCE_ID" \
    -H "Content-Type: application/json" -H "X-Hub-Signature-256: $SIG" -d "$BODY" >/dev/null 2>&1; then
  echo "FAIL: the signed webhook was refused by POST /ingest/{source_id}" >&2
  failures=$((failures + 1))
elif ! wait_for_status "$RUN2" "done" 120; then
  echo "FAIL: run 2 did not complete; it reads '$(run_status "$RUN2")': $(run_field "$RUN2" error)" >&2
  failures=$((failures + 1))
else
  # A COMPLETED run DOES publish its state -- this is the case the engine
  # handles, and it is the one the page reads.
  check "run 2's published status"  "$(state_of "$RUN2" status)"      "done"
  check "run 2's compensated"       "$(state_of "$RUN2" compensated)" ""
fi

echo
echo "==> run 3: the compensation itself fails, and must NOT read as compensated"
RUN3="$(start_run "{\"order_id\":\"ol-3\",\"email\":\"buyer@example.com\",\"source_id\":\"$SOURCE_ID\",\"items\":[{\"sku\":\"widget\",\"quantity\":1,\"price_cents\":2500}],\"simulate_payment_failure\":true,\"simulate_compensation_failure\":true}")"
if ! wait_for_status "$RUN3" failed 90; then
  echo "FAIL: run 3 did not reach failed; it reads '$(run_status "$RUN3")'" >&2
  failures=$((failures + 1))
else
  # The whole point: a compensation that RAN AND FAILED is not a compensated
  # step, and collapsing the two is how an order with a failed refund reads as
  # cleanly rolled back.
  err3="$(run_field "$RUN3" error)"
  contains "run 3's error" "$err3" "could not unwind: [reserve_inventory]"
  contains "run 3's error" "$err3" "unwound: []"
fi

# ---- the backend, and the page it serves -------------------------------
#
# The backend is what the README tells a reader to run, and the page's whole
# purpose is showing the compensation outcome -- so serving it and reading the
# same JSON the page reads is the difference between "the workflow compensates"
# and "you can see it compensate". The API key is passed in the environment;
# the page never holds one, which is the property backend/main.go's transport
# exists to guarantee.

echo
echo "==> starting the backend and its page"
(
  cd "$EXAMPLE_DIR" || exit 1
  # CLEAT_WEBHOOK_SOURCE_ID is the deployment's source, read by the backend and
  # attached server-side. It is not a per-request field: which source a
  # deployment's payments arrive through is a property of the deployment, and a
  # browser that could name any source could park an order on somebody else's.
  CLEAT_URL="$API" CLEAT_API_KEY="$API_KEY" CLEAT_WEBHOOK_SOURCE_ID="$SOURCE_ID" \
    go run ./backend -listen "127.0.0.1:$WEB_PORT" -web ./web
) >/tmp/ol-backend.log 2>&1 &
BACKEND_PID=$!
# No second trap: the EXIT trap above already kills BACKEND_PID, and replacing
# the trap here would drop cleanup's exit-status capture -- so a failure after
# this point would tear the stack down without dumping the logs that explain it.

WEB="http://127.0.0.1:$WEB_PORT"
deadline=$((SECONDS + 90))
until curl -fsS "$WEB/" >/dev/null 2>&1; do
  if (( SECONDS > deadline )); then
    echo "FAIL: the backend did not answer within 90s" >&2
    tail -30 /tmp/ol-backend.log >&2
    failures=$((failures + 1))
    break
  fi
  sleep 1
done

if (( failures == 0 )); then
  # The page and its stylesheet are both referenced by index.html and both
  # served under a CSP with no inline allowance -- a missing /app.css renders a
  # readable page that has silently lost its styling, so assert it is there.
  check "GET / serves the page" \
    "$(curl -fsS -o /dev/null -w '%{http_code}' "$WEB/")" "200"
  check "GET /app.css is served" \
    "$(curl -fsS -o /dev/null -w '%{http_code}' "$WEB/app.css")" "200"
  check "the page loads the script" \
    "$(curl -fsS "$WEB/" | grep -c 'src="/app.js"')" "1"

  # Start an order THROUGH the backend, the way the page does, and read back
  # the compensation through the same route the page polls.
  ORDER_ID="ol-web-$SUFFIX"
  STARTED="$(curl -fsS -X POST "$WEB/api/orders" \
    -H "Content-Type: application/json" -H "Idempotency-Key: ol-web-$SUFFIX" \
    -d "{\"order_id\":\"$ORDER_ID\",\"email\":\"buyer@example.com\",\"amount_cents\":2500,\"simulate_payment_failure\":true}" \
    | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
  if [[ -z "$STARTED" ]]; then
    echo "FAIL: POST /api/orders returned no run id" >&2
    failures=$((failures + 1))
  else
    # THE BACKEND CANNOT REPORT THIS THROUGH THE STATE, and that is #2520
    # again rather than a defect in the backend. A failed run's query state is
    # discarded before it reaches the database, so `GET /api/orders/{id}`
    # answers with an empty state for exactly the runs that compensate. What it
    # does carry is the run's `error`, which is where the workflow puts the
    # trail for that reason -- and it is what the page parses when the state is
    # empty. So the assertion is on the field that survives.
    get_order_field() {
      curl -fsS --max-time 10 "$WEB/api/orders/$1" 2>/dev/null |
        python3 -c 'import json,sys
try:
    d = json.load(sys.stdin)
    print(d.get(sys.argv[1], ""))
except Exception:
    print("")' "$2"
    }

    got=""
    deadline=$((SECONDS + 45))
    while (( SECONDS < deadline )); do
      got="$(get_order_field "$STARTED" error)"
      [[ "$got" == *"unwound: [reserve_inventory]"* ]] && break
      sleep 1
    done
    contains "the backend reports the compensation" "$got" "unwound: [reserve_inventory]"

    # And the other state, through the same route: an undo that failed must read
    # differently from one that succeeded.
    FAILED_ORDER="ol-web-fail-$SUFFIX"
    STARTED2="$(curl -fsS --max-time 15 -X POST "$WEB/api/orders" \
      -H "Content-Type: application/json" -H "Idempotency-Key: $FAILED_ORDER" \
      -d "{\"order_id\":\"$FAILED_ORDER\",\"email\":\"buyer@example.com\",\"amount_cents\":2500,\"simulate_payment_failure\":true,\"simulate_compensation_failure\":true}" \
      | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')"
    got2=""
    deadline=$((SECONDS + 45))
    while (( SECONDS < deadline )); do
      got2="$(get_order_field "$STARTED2" error)"
      [[ "$got2" == *"could not unwind: [reserve_inventory]"* ]] && break
      sleep 1
    done
    contains "the backend reports the failed unwind" "$got2" "could not unwind: [reserve_inventory]"

    # The idempotency key is the caller's to send and the backend's to forward:
    # the same key and payload must return the ORIGINAL run, not a second order.
    REPLAY="$(curl -fsS --max-time 15 -X POST "$WEB/api/orders" \
      -H "Content-Type: application/json" -H "Idempotency-Key: ol-web-$SUFFIX" \
      -d "{\"order_id\":\"$ORDER_ID\",\"email\":\"buyer@example.com\",\"amount_cents\":2500,\"simulate_payment_failure\":true}" \
      | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("id",""),d.get("idempotent_replay",False))')"
    check "a retried start replays rather than duplicating" \
      "$REPLAY" "$STARTED True"
  fi
fi

kill "$BACKEND_PID" 2>/dev/null || true

# ---- report ------------------------------------------------------------

echo
echo "dialect: $DIALECT"
echo "documented commands run: $ran of $total"

# A run that executed nothing is a broken check, not a clean tree -- the same
# arm build-documented-examples.sh carries. Two is the whole set here; anything
# less means the extractor or a guard above stopped doing its job.
if (( ran < total )); then
  echo "UNMEASURED: ran $ran of $total documented commands; a clean result would mean nothing." >&2
  exit 2
fi

if (( failures > 0 )); then
  echo >&2
  echo "$failures assertion(s) failed. The scenario does not behave as its README says." >&2
  exit 1
fi

echo "all assertions passed on $DIALECT"
