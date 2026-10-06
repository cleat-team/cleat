#!/usr/bin/env bash
#
# Run examples/ai-agent-platform end to end against a real worker, KILL the
# worker mid-loop, and assert that the resumed run did NOT re-ask the model.
#
# WHY THE KILL IS THE TEST, AND NOT AN EXTRA
#
# The property this scenario exists to demonstrate is the one the playbook calls
# its single most important line: a replayed agent run does not re-ask the
# model. `llm.chat` is registered with neither Idempotent nor SameValueOnReplay
# (plugins/llm/host_functions.go), so both are false -- which means the engine
# returns the RECORDED answer on replay instead of invoking again. That is
# simultaneously the cost control, the determinism guarantee and the audit
# trail, and it is the difference between a durable agent platform and a queue
# with a retry button.
#
# A green run that never crashes does not test any of it. So:
#
#   1. start a run and WAIT until the model has been asked everything it is
#      going to be asked -- observed as the model stub's request count reaching
#      the run's step count;
#   2. SIGKILL the worker, in the settle window between the last model call and
#      the end of the run;
#   3. restart it and let the run finish;
#   4. assert the count did not move.
#
# WHAT THE COUNT IS, AND WHY IT IS THE STUB'S
#
# The model provider is a local OpenAI-compatible stub, and it counts the
# requests it receives. After the kill, the resumed run replays the loop from
# event history: every `llm.chat` returns what was recorded, and the stub hears
# nothing. So the count after the run finishes must equal the count before the
# kill.
#
# THE COUNTER MUST BE ABLE TO MOVE, which is not the same as being able to read
# it. A stub that answered but never counted, or a counter read from the wrong
# place, would read a constant and pass the crash-resume assertion identically.
# So there is a KNOWN POSITIVE below: a SECOND run must move the count. That is
# what separates "the count is stable across a crash" from "the count is
# stable".
#
# THE FOUR HITCH POINTS ARE EACH ASSERTED, not merely present:
#
#   host functions    the model call, and the crash-resume count above all
#   HTTP routes       the approval arrives as an event published to a
#                     plugin-registered route, and the run does not proceed
#                     without it
#   edge middleware   a limit seeded through the PLUGIN's own route, then a
#                     burst, then a check that the 429 carried the plugin's
#                     X-RateLimit-Limit and not the core limiter's bare 429
#   background loop   ratelimiter's reload, which reads every tenant's limits
#                     with plugin.AcrossAllTenants by name -- asserted by the
#                     limit FIRING, since without the reload the buckets are
#                     empty and a registered middleware cannot fire at all
#
# DERIVED, NOT LISTED
#
# The `cleat` commands come out of the example's README verbatim, the way
# build-documented-examples.sh takes its `cleat build`. See
# run-order-lifecycle-scenario.sh for the substitutions and why they are not a
# lie about the README.
set -uo pipefail

# ERREXIT OFF, EXPLICITLY -- as a DECLARATION, not as a fix for anything.
# Several steps read a non-zero exit as DATA, and the wait helpers loop until a
# condition holds rather than asserting on the first try.
set +e

cd "$(git rev-parse --show-toplevel)" || exit 2

EXAMPLE_DIR="examples/ai-agent-platform"
README="$EXAMPLE_DIR/README.md"

CLEAT_BIN="${CLEAT_BIN:-$(pwd)/.bin/cleat}"
if [[ ! -x "$CLEAT_BIN" ]]; then
  echo "UNMEASURED: no cleat binary at $CLEAT_BIN. Build it first:" >&2
  echo "  go build -o .bin/cleat ./cmd/cleat" >&2
  exit 2
fi

# A SECOND BINARY, and it is not interchangeable with the first.
# `egress-allow` is a cleatctl subcommand -- it writes the per-tenant allowlist
# a host-function call needs. Running it against `cleat` prints cleat's usage
# and exits non-zero, which reads as "the grant was refused" rather than "that
# subcommand does not live here". Refused up front for the same reason as the
# binary above: a missing tool is a failure of the job, not a finding about the
# example.
CLEATCTL_BIN="${CLEATCTL_BIN:-$(pwd)/.bin/cleatctl}"
if [[ ! -x "$CLEATCTL_BIN" ]]; then
  echo "UNMEASURED: no cleatctl binary at $CLEATCTL_BIN. Build it first:" >&2
  echo "  go build -o .bin/cleatctl ./cmd/cleatctl" >&2
  exit 2
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "UNMEASURED: docker is not available, so nothing here can be run." >&2
  echo "This is a failure of the job, not a finding about the example." >&2
  exit 2
fi

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

examined=0
for c in "$BUILD_CMD" "$DEPLOY_CMD"; do [[ -z "$c" ]] || examined=$((examined + 1)); done
if [[ $examined -lt 2 ]]; then
  echo "UNMEASURED: found $examined of 2 documented cleat commands (build, deploy) in $README." >&2
  echo "The extractor stopped seeing them, or the README stopped documenting them. Either" >&2
  echo "way a clean result below would mean nothing. Extracted:" >&2
  printf '  build : %s\n  deploy: %s\n' "$BUILD_CMD" "$DEPLOY_CMD" >&2
  exit 2
fi

# ---- environment -------------------------------------------------------

free_port() {
  python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
}

SUFFIX="$$-$(date +%s)"
PG_PORT="$(free_port)"
API_PORT="$(free_port)"
STUB_PORT="$(free_port)"
WORKER_IMAGE="cleat-ai-agent-platform-test:$SUFFIX"
COMPOSE=(docker compose -f "$EXAMPLE_DIR/docker-compose.yml")
OUT_DIR="$(mktemp -d)"
export CLEAT_PG_PORT="$PG_PORT" CLEAT_API_PORT="$API_PORT" CLEAT_MODEL_STUB_PORT="$STUB_PORT"
export COMPOSE_PROJECT_NAME="cleat-ai-agent-platform-$SUFFIX"

# On failure the worker's own log is the only artefact that explains what
# happened, and the stack is about to be removed -- so it is captured HERE. A
# separate CI step for this would find nothing: by the time it runs, the
# containers are gone.
cleanup() {
  local rc=$?
  if (( rc != 0 )); then
    echo >&2
    # SINCE THE RESTART WHEN THERE WAS ONE. This dumped `--tail=60`
    # unconditionally in the sibling scenario until 2026-09-28, and after a
    # restart those 60 lines are the pre-kill startup burst -- so the failure
    # that mattered was reported with a perfectly healthy worker's log attached
    # to it (cleat#2562's CI run).
    if [[ -n "${RESTART_AT:-}" ]]; then
      echo "--- cleat-worker (since the $RESTART_AT restart) ---" >&2
      "${COMPOSE[@]}" logs --since "$RESTART_AT" cleat-worker >&2 2>&1 || true
    else
      echo "--- cleat-worker (tail) ---" >&2
      "${COMPOSE[@]}" logs --tail=60 cleat-worker >&2 2>&1 || true
    fi
    echo "--- model-stub (tail) ---" >&2
    "${COMPOSE[@]}" logs --tail=10 model-stub >&2 2>&1 || true
  fi
  # The backend is started late and lives on the host, so it is killed HERE
  # rather than by a trap of its own. A second `trap ... EXIT` REPLACES this
  # one -- the shell keeps one handler per signal -- so setting one where the
  # backend starts would silently drop the stack teardown and the log dump
  # above with it. `disown` is not needed: this shell has no job control.
  if [[ -n "${BACKEND_PID:-}" ]]; then
    kill "$BACKEND_PID" 2>/dev/null || true
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

echo "==> building the worker image from this checkout"
if ! docker build -t "$WORKER_IMAGE" . >/tmp/aap-docker-build.log 2>&1; then
  echo "FAIL: docker build of the repository's Dockerfile" >&2
  tail -30 /tmp/aap-docker-build.log >&2
  exit 1
fi

echo "==> docker compose up"
if ! "${COMPOSE[@]}" up -d >/tmp/aap-compose-up.log 2>&1; then
  echo "FAIL: docker compose up" >&2
  tail -30 /tmp/aap-compose-up.log >&2
  exit 1
fi

API="http://localhost:$API_PORT"
STUB="http://localhost:$STUB_PORT"
deadline=$((SECONDS + 120))
until curl -fsS --max-time 5 "$API/healthz" >/dev/null 2>&1; do
  if (( SECONDS > deadline )); then
    echo "FAIL: the worker did not answer /healthz within 120s" >&2
    "${COMPOSE[@]}" logs --tail=60 cleat-worker >&2 || true
    exit 1
  fi
  sleep 2
done

# The log is read ONCE into WORKER_LOG and both facts are taken from it -- the
# API key and the tenant it belongs to. Two greps over two reads would be two
# reads of a growing stream, so the key and the tenant could come from different
# moments; one read cannot disagree with itself.
API_KEY=""
deadline=$((SECONDS + 30))
while (( SECONDS < deadline )); do
  WORKER_LOG="$("${COMPOSE[@]}" logs cleat-worker 2>/dev/null || true)"
  API_KEY="$(printf '%s' "$WORKER_LOG" | grep -oE 'cleat_sk_[0-9a-f]+' | head -1 || true)"
  [[ -n "$API_KEY" ]] && break
  sleep 1
done
if [[ -z "$API_KEY" ]]; then
  echo "FAIL: the worker printed no API key; nothing below can authenticate." >&2
  "${COMPOSE[@]}" logs --tail=60 cleat-worker >&2 || true
  exit 1
fi
auth=(-H "Authorization: Bearer $API_KEY")

# THE TENANT COMES FROM THE WORKER'S OWN BANNER, not from a literal here.
#
# It is not decoration: the tenant in the banner is the one the auto-generated
# API key belongs to, and the EGRESS GRANT BELOW is per tenant. A literal that
# disagreed with the key would grant egress to a tenant that never makes the
# call, and every run would still be refused -- with a message about the
# allowlist, which is the half that was already right. Read, not assumed.
TENANT="$(printf '%s' "$WORKER_LOG" | grep -oE 'Tenant ID: [0-9a-fA-F-]{36}' | head -1 | awk '{print $3}')"
if [[ -z "$TENANT" ]]; then
  echo "FAIL: the worker printed no 'Tenant ID:' line, so the egress grant below" >&2
  echo "      has no tenant to name and every run would be refused at step one." >&2
  exit 1
fi

# The model stub holds the assertion for this whole scenario. If it is not
# answering, every count below reads as "the model was never asked" and the
# crash-resume check passes vacuously -- so this is checked FIRST, before
# anything else can be believed.
model_hits() {
  curl -fsS --max-time 5 "$STUB/" 2>/dev/null |
    python3 -c 'import sys,json; print(json.load(sys.stdin)["requests"])' 2>/dev/null || echo ""
}
if [[ -z "$(model_hits)" ]]; then
  echo "FAIL: the model stub does not answer on $STUB." >&2
  echo "EVERY COUNT BELOW WOULD READ AS ZERO, and a crash-resume assertion over a" >&2
  echo "counter that cannot move passes identically to a real one." >&2
  "${COMPOSE[@]}" logs --tail=30 model-stub >&2 || true
  exit 1
fi

# ---- the documented commands -------------------------------------------

ran=0
total=2

run_documented() {
  local label="$1" cmd="$2"
  echo
  echo "==> ($label) \$ $cmd"
  case "$cmd" in
    *"$CLEAT_BIN"*) ;;
    *) echo "FAIL: the extracted $label command is not a cleat invocation: $cmd" >&2; exit 1 ;;
  esac
  if ! eval "$cmd" >/tmp/aap-cmd.log 2>&1; then
    echo "FAIL: the README's documented command failed:" >&2
    tail -25 /tmp/aap-cmd.log >&2
    exit 1
  fi
  tail -3 /tmp/aap-cmd.log
  ran=$((ran + 1))
}

BUILD_RUN="${BUILD_CMD//cleat /$CLEAT_BIN }"
BUILD_RUN="${BUILD_RUN//-o \/tmp\/out/-o $OUT_DIR}"
run_documented build "$BUILD_RUN"

WASM="$OUT_DIR/ai-agent-platform.wasm"
if [[ ! -f "$WASM" ]]; then
  echo "FAIL: the documented build did not produce $WASM." >&2
  echo "The artifact is named for cleat.yaml's own 'name:' (cleat#2692), not the directory." >&2
  ls -la "$OUT_DIR" >&2
  exit 1
fi

DEPLOY_RUN="${DEPLOY_CMD//cleat /$CLEAT_BIN }"
DEPLOY_RUN="${DEPLOY_RUN//\/tmp\/out/$OUT_DIR}"
DEPLOY_RUN="${DEPLOY_RUN//localhost:5432/localhost:$PG_PORT}"
run_documented deploy "$DEPLOY_RUN"

# ---- the tenant's egress grant -----------------------------------------
#
# EGRESS NEEDS TWO PERMISSIONS AND THE COMPOSE ONLY SUPPLIES ONE. Without this
# the run fails at step one with:
#
#   Post "http://model-stub:9100/v1/chat/completions": egress to model-stub is
#   refused by cleat's network policy: host is not on this tenant's egress allowlist
#
# which names the ALLOWLIST -- so it does not send you to the private-address
# flag the compose already sets, and the flag's presence reads as proof it was
# handled.
#
# WHY THIS SCENARIO NEEDS IT AND examples/integration-hub DOES NOT, given both
# permit one private host by name: that scenario's delivery is made by a
# plugin's BACKGROUND LOOP, which runs with no tenant in context and therefore
# answers to the operator list alone. The model call here is a HOST FUNCTION
# inside a run, so it has a tenant, so cmd/cleat-worker/plugin_egress.go's
# AllowHost runs and the tenant's own list must permit the host. Same flag, same
# host class, different answer -- the difference is the shape of the caller.
echo
echo "==> granting the tenant egress to the model stub"
EGRESS_DSN="postgres://cleat:cleat@localhost:$PG_PORT/cleat?sslmode=disable"
if ! "$CLEATCTL_BIN" --db "$EGRESS_DSN" egress-allow add "$TENANT" model-stub >/tmp/aap-egress.log 2>&1; then
  echo "FAIL: could not grant the tenant egress to model-stub." >&2
  tail -10 /tmp/aap-egress.log >&2
  exit 1
fi
# Read it back rather than trusting the write. `egress-allow add` on a tenant
# that already has rows is a merge, and its own output is the only place the
# resulting list is visible -- a grant that did not take looks identical to one
# that did until a run is refused.
if ! "$CLEATCTL_BIN" --db "$EGRESS_DSN" egress-allow list "$TENANT" 2>/dev/null | grep -q 'model-stub'; then
  echo "FAIL: the grant did not take -- model-stub is not on $TENANT's list." >&2
  "$CLEATCTL_BIN" --db "$EGRESS_DSN" egress-allow list "$TENANT" >&2 2>&1 || true
  exit 1
fi
echo "    ok      $TENANT may reach model-stub"

# ---- starting a run ----------------------------------------------------

# start_run posts the start body and echoes the run id.
#
# TWO THINGS ABOUT THE SHAPE ARE NOT GUESSABLE, and both were wrong on the first
# run of this script:
#
#   * the workflow's own fields go under a wrapper key, `input` -- the start
#     endpoint takes `{"input":{...}}`, not the fields at the top level. A start
#     that omits the wrapper is refused, and the failure reads as a bad workflow
#     rather than as a bad request.
#   * the id comes back as `id`, not `workflow_id`.
#
# The tenant is resolved from the API KEY by the worker; `tenant_id` inside the
# input is the workflow's own label for the run and is what the spend ceiling is
# reported against. It is passed explicitly because AgentInput requires it.
#
# THE IDEMPOTENCY KEY IS DIFFERENT PER CALL, and that is load-bearing rather
# than tidy: this script starts four runs, and a shared key would make the
# second onwards be refused -- the same key with a different task is exactly the
# collision the guard exists to catch -- so every count below would be
# describing one run while appearing to describe four.
#
# THE SEQUENCE IS A PARAMETER, NOT A COUNTER, and that is not a style choice.
# The first version kept `RUN_SEQ` in the shell and incremented it inside this
# function -- but every call site is `RUN_ID="$(start_run ...)"`, and a command
# substitution runs in a SUBSHELL, so the increment was discarded every time and
# all four calls sent `-$SUFFIX-1`. Run 1 created a run; runs 2, 3 and 4 reused
# its key with a different task and were refused 409, which surfaced as "the
# second run did not start" -- a message about the run, not about the key.
# Fourth argument, so the caller owns the identity and no subshell can lose it.
start_run() {
  local budget="$1" task="$2" max_steps="$3" seq="$4"
  curl -fsS --max-time 15 -X POST "$API/api/workflows/ai-agent-platform/start" \
    "${auth[@]}" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: aap-$SUFFIX-$seq" \
    -d "{\"input\":{\"tenant_id\":\"$TENANT\",\"task\":\"$task\",\"max_steps\":$max_steps,\"budget_usd\":$budget,\"artifact_key\":\"reports/nightly.md\"}}" \
    2>/dev/null | python3 -c 'import sys,json
print(json.load(sys.stdin).get("id",""))' 2>/dev/null || echo ""
}

# query_state reads ONE key from the run's live query state, which the workflow
# writes as it goes -- and is therefore readable WHILE the run is mid-flight,
# unlike its final status.
#
# It uses the `?key=` form rather than reading the whole workflow object,
# deliberately: that endpoint exists for exactly this and returns
# {"key":...,"value":...} (cmd/cleat-worker/server.go, handleGetWorkflow), so
# the read does not depend on the shape of the object the other branch returns
# -- which is engine.WorkflowInstance whole, every tag included. A guessed tag
# reads as an empty string, which is the same thing every wait loop below reads
# as "not yet".
query_state() {
  curl -fsS --max-time 10 "$API/api/workflows/$1?key=$2" "${auth[@]}" 2>/dev/null |
    python3 -c 'import sys,json
try: print(json.load(sys.stdin).get("value",""))
except Exception: print("")' 2>/dev/null || echo ""
}

# run_state is the summary printed on failure. It reads the SAME source as the
# assertions, so a failure message can never report a number the checks did not
# use -- two readers of one fact, not two facts.
run_state() {
  printf 'status=%s steps=%s spent=%s' \
    "$(query_state "$1" status)" "$(query_state "$1" agent_step)" "$(query_state "$1" agent_cost)"
}

echo
echo "==> starting a run"
RUN_ID="$(start_run 1.00 "Summarise yesterday's incidents." 6 1)"
if [[ -z "$RUN_ID" ]]; then
  echo "FAIL: the run did not start; nothing below can be measured." >&2
  exit 1
fi
echo "    run $RUN_ID"

# ---- waiting for the model calls to be done ----------------------------
#
# THE KILL HAS TO LAND AFTER THE MODEL CALLS AND BEFORE THE RUN ENDS. The
# workflow sleeps 30s (SettleDelayMs) between its last model call and its
# result, so the window is wide and the signal is exact: the stub's count
# reaching its final value means the loop is over and the run is settling.
HITS_BEFORE=""
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  n="$(model_hits)"
  if [[ -n "$n" ]] && (( n >= 2 )); then
    HITS_BEFORE="$n"
    break
  fi
  sleep 1
done
if [[ -z "$HITS_BEFORE" ]]; then
  echo "FAIL: the model was never asked (count=$n), so there is no loop to crash inside." >&2
  echo "THE CRASH-RESUME ASSERTION WAS NOT EVALUATED." >&2
  echo "    run state: $(run_state "$RUN_ID")" >&2
  exit 1
fi
echo "    the model was asked $HITS_BEFORE time(s); the run is in its settle window"

# ---- SIGKILL -----------------------------------------------------------

echo
echo "==> SIGKILLing the worker mid-run"
# SIGKILL, not `stop`: a graceful stop drains the run (--shutdown-grace) and
# the run would finish, which is the opposite of the thing being tested.
"${COMPOSE[@]}" kill -s SIGKILL cleat-worker >/dev/null 2>&1
echo "    killed; the run is now owned by a worker that does not exist"

# ---- restart -----------------------------------------------------------

RESTART_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
"${COMPOSE[@]}" up -d cleat-worker >/tmp/aap-restart.log 2>&1 || true

deadline=$((SECONDS + 180))
until curl -fsS --max-time 5 "$API/healthz" >/dev/null 2>&1; do
  if (( SECONDS > deadline )); then
    echo "FAIL: the worker did not come back within 180s." >&2
    echo "THE CRASH-RESUME ASSERTION WAS NOT EVALUATED, and that is not the same" >&2
    echo "thing as passing. A worker that never resumes proves nothing about" >&2
    echo "resumption, and reporting a green here would be the exact false result" >&2
    echo "this scenario exists to avoid." >&2
    "${COMPOSE[@]}" ps -a >&2 2>&1 || true
    "${COMPOSE[@]}" logs --since "${RESTART_AT:-5m}" cleat-worker >&2 2>&1 || true
    exit 1
  fi
  sleep 2
done
echo "    the worker is back"

# ---- the assertion -----------------------------------------------------
#
# Wait for the run to finish, then compare the model's count against what it
# was before the kill. THIS IS THE SCENARIO'S CLAIM.
echo
echo "==> waiting for the resumed run to finish"
FINAL_STATUS=""
deadline=$((SECONDS + 120))
while (( SECONDS < deadline )); do
  FINAL_STATUS="$(query_state "$RUN_ID" status)"
  [[ "$FINAL_STATUS" == "done" ]] && break
  sleep 2
done
if [[ "$FINAL_STATUS" != "done" ]]; then
  echo "FAIL: the run did not reach 'done' after resume; it reads '$FINAL_STATUS'." >&2
  echo "    run state: $(run_state "$RUN_ID")" >&2
  echo "THE CRASH-RESUME ASSERTION WAS NOT EVALUATED." >&2
  exit 1
fi

HITS_AFTER="$(model_hits)"
STEPS="$(query_state "$RUN_ID" agent_step)"
echo "    model requests before the kill: $HITS_BEFORE"
echo "    model requests after  the kill: $HITS_AFTER"
echo "    steps the run reports:          $STEPS"

if [[ -z "$HITS_AFTER" ]]; then
  echo "FAIL: the model stub stopped answering, so the count could not be read." >&2
  exit 1
fi
if (( HITS_AFTER != HITS_BEFORE )); then
  echo "FAIL: the model was asked $((HITS_AFTER - HITS_BEFORE)) more time(s) after the crash." >&2
  echo "A resumed run must return the RECORDED answer for every llm.chat it already" >&2
  echo "made. This is the cost the playbook claims the engine removes, and it is not" >&2
  echo "being removed: every resumed agent run would re-pay for its whole context." >&2
  exit 1
fi
# The count must also agree with the run's own idea of how many steps it took.
# Two numbers from different places -- the stub's counter and the workflow's
# query state -- and they have to reconcile.
if [[ -n "$STEPS" ]] && (( HITS_BEFORE != STEPS )); then
  echo "FAIL: the model was asked $HITS_BEFORE time(s) but the run reports $STEPS step(s)." >&2
  echo "These come from different places (the provider's counter and the workflow's" >&2
  echo "query state), so a disagreement means one of them is not describing this run." >&2
  exit 1
fi
echo "OK: the resumed run re-asked nothing"

# ---- known positive: the counter must be able to move -------------------
echo
echo "==> known positive: a second run must move that count"
RUN2="$(start_run 1.00 "A second task, to prove the counter moves." 6 2)"
if [[ -z "$RUN2" ]]; then
  echo "FAIL: the second run did not start." >&2
  exit 1
fi
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  [[ "$(query_state "$RUN2" status)" == "done" ]] && break
  sleep 2
done
HITS_2="$(model_hits)"
echo "    model requests after the second run: $HITS_2"
if [[ -z "$HITS_2" ]] || (( HITS_2 <= HITS_AFTER )); then
  echo "FAIL: a second run did not move the counter ($HITS_AFTER -> $HITS_2)." >&2
  echo "A counter that cannot move makes the assertion above vacuous: 'the count is" >&2
  echo "unchanged across a crash' and 'the count is always this' are the same reading," >&2
  echo "and only this check tells them apart." >&2
  exit 1
fi

# ---- the spend ceiling -------------------------------------------------
#
# The claim is that a run STOPS at its ceiling rather than reporting it
# afterwards. The observable is the status AND the number of model calls the
# run made: the stub's counter delta is bounded by the budget.
echo
echo "==> the per-run spend ceiling"

# THE BUDGET IS DERIVED FROM A MEASURED COST, NOT PICKED.
#
# Two model calls have been made by the run above, and the workflow publishes
# what they cost. Guessing a dollar figure here would have been wrong by an
# order of magnitude, and the first version of this section did exactly that:
# the cost of an unknown model name is computed by the openai provider's
# `default:` branch USING gpt-4o's prices (plugins/llm/providers/openai.go), so
# a stub model is priced at $2.50/M prompt tokens -- around $0.0006 a call at
# this stub's usage, not the cent the budget assumed.
#
# Half of one call's cost means the FIRST call already reaches the ceiling, so
# the bounded run must make exactly one model call. That is the assertion, and
# it self-calibrates against whatever the provider reports.
SPENT_2CALLS="$(query_state "$RUN_ID" agent_cost)"
HALF_A_CALL="$(python3 -c '
import sys
try:
    spent = float(sys.argv[1])
except (ValueError, IndexError):
    sys.exit(0)
print("%.9f" % max(spent / 4.0, 1e-9))' "$SPENT_2CALLS" 2>/dev/null)"
if [[ -z "$HALF_A_CALL" ]]; then
  echo "FAIL: the first run published no spend ($SPENT_2CALLS), so a ceiling" >&2
  echo "      derived from it cannot be set. The budget check has nothing to" >&2
  echo "      compare against either, so this is not only a test problem." >&2
  exit 1
fi
echo "    two calls cost \$$SPENT_2CALLS, so the ceiling below is \$$HALF_A_CALL (half of one)"

BEFORE_BUDGET="$(model_hits)"
RUN3="$(start_run "$HALF_A_CALL" 'Anything, with a budget too small to complete.' 6 3)"
if [[ -z "$RUN3" ]]; then
  echo "FAIL: the budgeted run did not start." >&2
  exit 1
fi
BUDGET_STATUS=""
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  BUDGET_STATUS="$(query_state "$RUN3" status)"
  # done OR budget_exceeded both end the run; anything else keeps waiting.
  [[ "$BUDGET_STATUS" == "done" || "$BUDGET_STATUS" == "budget_exceeded" ]] && break
  sleep 2
done
AFTER_BUDGET="$(model_hits)"
BUDGETED_CALLS=$((AFTER_BUDGET - BEFORE_BUDGET))
echo "    status: $BUDGET_STATUS (model calls: $BUDGETED_CALLS, want 1)"
if [[ "$BUDGET_STATUS" != "budget_exceeded" ]]; then
  echo "FAIL: a run with a \$$HALF_A_CALL budget finished as '$BUDGET_STATUS', not budget_exceeded." >&2
  echo "Either the ceiling is not enforced, or the cost the provider reported is not" >&2
  echo "reaching the loop -- which would make the ceiling a decoration." >&2
  exit 1
fi
# THE STATUS IS NOT THE BOUND. A loop that spent everything and then reported the
# ceiling would produce the same status having made six calls; `budget_exceeded`
# on its own says only that the ceiling was noticed.
if (( BUDGETED_CALLS != 1 )); then
  echo "FAIL: the ceiling was \$$HALF_A_CALL -- less than one call -- and the run" >&2
  echo "      still made $BUDGETED_CALLS model calls, want 1." >&2
  echo "The check is not between the loop and the paid call it is meant to bound." >&2
  exit 1
fi

# ---- the human wait, over an HTTP route --------------------------------
#
# The approval arrives as an event published to a route the event-triggers
# plugin registers. This is the scenario's HTTP-routes hitch point: the run
# blocks until something outside it posts here.
echo
echo "==> the human wait, satisfied through a plugin route"
RUN4="$(start_run 1.00 'Approve the rollback before answering.' 2 4)"
if [[ -z "$RUN4" ]]; then
  echo "FAIL: the approval run did not start." >&2
  exit 1
fi
# Wait for it to be waiting, so the publish lands on a run that is actually
# parked rather than racing it.
PARKED=""
deadline=$((SECONDS + 60))
while (( SECONDS < deadline )); do
  if [[ "$(query_state "$RUN4" status)" == "awaiting_approval" ]]; then PARKED=1; break; fi
  if [[ "$(query_state "$RUN4" status)" == "done" ]]; then break; fi
  sleep 1
done
if [[ -z "$PARKED" ]]; then
  echo "    (the run was not observed waiting; it may have finished before a decision was needed)"
fi

EVENT_ID="$(python3 -c 'import uuid;print(uuid.uuid4())')"
if ! curl -fsS --max-time 15 -X POST "$API/api/events/publish" "${auth[@]}" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"$EVENT_ID\",\"event_type\":\"agent.approval\",\"data\":{\"approved\":true,\"note\":\"ship it\"}}" \
  >/tmp/aap-publish.log 2>&1; then
  echo "FAIL: the approval event was refused by POST /api/events/publish." >&2
  tail -10 /tmp/aap-publish.log >&2
  echo "This is the HTTP-routes hitch point; without it the run waits forever and" >&2
  echo "the wait is a step that cannot be satisfied from outside the worker." >&2
  exit 1
fi

APPROVAL_STATUS=""
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  APPROVAL_STATUS="$(query_state "$RUN4" status)"
  [[ "$APPROVAL_STATUS" == "done" ]] && break
  sleep 2
done
echo "    status after the approval: $APPROVAL_STATUS"
if [[ "$APPROVAL_STATUS" != "done" ]]; then
  echo "FAIL: the run did not finish after its approval was published (reads '$APPROVAL_STATUS')." >&2
  exit 1
fi
# THE RUN FINISHING IS NOT EVIDENCE THE APPROVAL WAIT WAS ENTERED. A workflow
# that never reached `request_approval` would also finish, and this section
# would then pass while the HTTP-routes hitch point went untested -- the publish
# accepted by a worker that no run was waiting on.
#
# `approval_wait` is written on ENTRY to requestApproval (agent.go), before any
# poll, so a run whose state carries it demonstrably entered the wait. Note it
# is NOT `approval_polls`, which is written only on the miss path and is
# therefore empty for a run approved on its first poll -- the case here.
WAIT_ENTERED="$(query_state "$RUN4" approval_wait)"
if [[ "$WAIT_ENTERED" != "entered" ]]; then
  echo "FAIL: the run finished but never entered the approval wait (approval_wait='$WAIT_ENTERED')." >&2
  echo "The publish was accepted, but no run was waiting on it, so the HTTP-routes" >&2
  echo "hitch point was not exercised by this section." >&2
  exit 1
fi
echo "    the wait was entered (polls before the decision: $(query_state "$RUN4" approval_polls))"

# ---- the app, and the page's own vocabulary ----------------------------
#
# The README documents `go run ./backend` under "The app", so it is a documented
# command and it runs here. The page matters for a second reason: NOTHING ELSE
# IN THIS SCENARIO EXECUTES ITS JAVASCRIPT. Every assertion above reads JSON the
# worker produced, so a wrong branch in app.js is invisible to all of them.
echo
echo "==> starting the backend and its page"
WEB_PORT="$(free_port)"
(
  cd "$EXAMPLE_DIR" || exit 1
  CLEAT_URL="$API" CLEAT_API_KEY="$API_KEY" \
    go run ./backend -listen "127.0.0.1:$WEB_PORT" -web ./web
) >/tmp/aap-backend.log 2>&1 &
BACKEND_PID=$!
# NO TRAP HERE, deliberately: the EXIT trap set above already tears the stack
# down, and the shell keeps ONE handler per signal, so a second `trap ... EXIT`
# would replace it rather than add to it -- dropping the teardown and the
# failure log dump together. cleanup kills BACKEND_PID itself.

WEB="http://127.0.0.1:$WEB_PORT"
deadline=$((SECONDS + 90))
until curl -fsS "$WEB/" >/dev/null 2>&1; do
  if (( SECONDS > deadline )); then
    echo "FAIL: the backend did not answer within 90s" >&2
    tail -30 /tmp/aap-backend.log >&2
    exit 1
  fi
  sleep 1
done
echo "    ok      the page is served"

# The page's two assets, which nothing else here would notice. A page whose
# script 404s renders as a blank form: the node in index.html still arrives, so
# a check on `/` alone says the page is served and means only that the HTML is.
for asset in app.js app.css; do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$WEB/$asset" 2>/dev/null)"
  if [[ "$code" != "200" ]]; then
    echo "FAIL: the page's $asset answered $code, want 200." >&2
    echo "The page would render as a blank form while / still returned 200." >&2
    exit 1
  fi
done
echo "    ok      app.js and app.css are served"

# THE PAGE'S STATUS VOCABULARY IS CHECKED AGAINST THE CODE THAT WRITES IT.
#
# This scenario has NINE run states, and the page paints each one. A `case` label
# the workflow never writes is a branch that can never be taken -- a colour a run
# will never get, which reads as "that state does not happen" rather than as a
# typo. (The sibling scenario shipped `case "completed"` for a vocabulary whose
# finished state is `done`, so every finished run painted as the fallback.)
#
# It is a SUBSTRING test, deliberately loose: the question is whether the word
# appears in the code that writes it, not how it is spelled there. An empty
# extraction FAILS rather than passing, so a refactor that stops this finding the
# case labels reports itself instead of going quiet.
missing="$(python3 - "$EXAMPLE_DIR/web/app.js" <<'PY'
import pathlib, re, sys
page = pathlib.Path(sys.argv[1]).read_text()
cases = sorted(set(re.findall(r'case "([a-z_]+)":', page)))
if not cases:
    print("EXTRACTED-NOTHING-FROM-THE-PAGE")
    raise SystemExit
sources = " ".join(p.read_text() for p in [
    pathlib.Path("engine/status_vocabulary.go"),        # the RUN vocabulary
    pathlib.Path("examples/ai-agent-platform/agent.go"),  # the query-state values
])
print(",".join(c for c in cases if f'"{c}"' not in sources and f"'{c}'" not in sources))
PY
)"
if [[ -n "$missing" ]]; then
  echo "FAIL: the page paints status(es) the workflow never writes: $missing" >&2
  echo "A run can never be in that state, so that branch is a colour nothing reaches." >&2
  exit 1
fi
echo "    ok      every status the page paints is one the workflow writes"

# The page's own route, through the backendkit proxy. A run started THROUGH the
# app is the whole page path in one call.
# The task text is what selects the approval path in the model stub -- see the
# `elif 'approve' in task` branch in docker-compose.yml. Without something that
# reaches `request_approval` there is no run to approve, and the approve button
# below would be reported as working on a 409.
APP_RUN="$(curl -fsS --max-time 15 -X POST "$WEB/api/agents" \
  -H 'Content-Type: application/json' -H "Idempotency-Key: aap-$SUFFIX-web" \
  -d '{"tenant_id":"'"$TENANT"'","task":"Decide whether to approve the rollback.","max_steps":3,"budget_usd":1.00}' \
  2>/dev/null |
  python3 -c 'import json,sys
print(json.load(sys.stdin).get("id",""))' 2>/dev/null || echo "")"
if [[ -z "$APP_RUN" ]]; then
  echo "FAIL: a run started through the app returned no id." >&2
  tail -20 /tmp/aap-backend.log >&2
  exit 1
fi
echo "    ok      a run started through the app ($APP_RUN)"

# Wait until the run is genuinely parked before deciding. The app's OWN route
# reports the wait, and the approve route refuses 409 unless the run reads
# `awaiting_approval` -- see approveAgent, whose guard is real and was found by
# this section failing when it raced the wait.
app_state() {
  curl -fsS --max-time 10 "$WEB/api/agents/$APP_RUN" 2>/dev/null |
    python3 -c 'import json,sys
d=json.load(sys.stdin)
print((d.get("state") or {}).get("status",""))' 2>/dev/null || echo ""
}
PARKED=""
deadline=$((SECONDS + 60))
while (( SECONDS < deadline )); do
  if [[ "$(app_state)" == "awaiting_approval" ]]; then PARKED=1; break; fi
  sleep 1
done
if [[ -z "$PARKED" ]]; then
  echo "FAIL: the run started through the app never reached awaiting_approval" >&2
  echo "      (reads '$(app_state)'), so there is nothing for the button to decide." >&2
  exit 1
fi
echo "    ok      the app reports it parked"

# The approval, through the APP'S OWN route rather than the worker's -- which is
# the point: the browser must not hold the API key, so the publish happens
# server-side and the page only sees the button.
if ! curl -fsS --max-time 15 -X POST "$WEB/api/agents/$APP_RUN/approve" \
  -H 'Content-Type: application/json' -d '{"approved":true,"note":"via the app"}' \
  >/dev/null 2>&1; then
  echo "FAIL: the app's approval route did not accept a decision." >&2
  exit 1
fi
echo "    ok      the app's approval route published the decision"

# AND THE RUN MUST ACT ON IT. A publish that nothing reads looks identical to a
# successful approval from the route's side -- it answers 202 because the event
# is published, not because a run moved. So the assertion is on the run.
APP_DONE=""
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  [[ "$(app_state)" == "done" ]] && { APP_DONE=1; break; }
  sleep 2
done
if [[ -z "$APP_DONE" ]]; then
  echo "FAIL: the run approved through the app did not finish (reads '$(app_state)')." >&2
  echo "The event was published and no run consumed it, so the approval path is" >&2
  echo "open at the publish end and closed at the wait end." >&2
  exit 1
fi
echo "    ok      the run approved through the app finished"

# The run list the page reads must include the run just started through it. This
# is the app's own reader, so it is a second opinion on the same fact rather
# than a re-read of the one above.
listed="$(curl -fsS --max-time 10 "$WEB/api/agents" 2>/dev/null |
  python3 -c 'import json,sys
d=json.load(sys.stdin)
rows=d.get("runs") or d.get("agents") or []
print(sum(1 for r in rows if sys.argv[1] in json.dumps(r)))' "$APP_RUN" 2>/dev/null || echo "-1")"
if [[ "$listed" != "1" ]]; then
  echo "FAIL: the app's run list does not contain the run started through it (found $listed)." >&2
  exit 1
fi
echo "    ok      the app's run list contains it"

# ---- the edge middleware + the background loop -------------------------
#
# THIS SECTION RUNS LAST, AND THAT IS NOT ARRANGEMENT.
#
# It is the only DESTRUCTIVE part of the scenario: it seeds a real 5-per-60s
# limit on this tenant and then bursts until it fires, which exhausts the
# tenant's quota for the rest of the minute. Run before the app section, it made
# `POST /api/agents` answer 429 -- and that surfaced as "a run started through
# the app returned no id", a message about the app when the app was fine. The
# sibling scenario (examples/integration-hub) was reordered for the same reason
# after the same failure.
#
# Two mechanisms, asserted together because the SECOND is what makes the first
# able to fire: ratelimiter builds its buckets from rows in its own table,
# refreshed by its background loop running plugin.AcrossAllTenants. A
# deployment that never writes a row has a registered middleware that cannot
# fire, so "the limit fired" is evidence for both.
#
# The header is the discriminator. BOTH the core limiter and the plugin's
# answer 429 with the body {"error":"rate limit exceeded"}; only the plugin's
# carries X-RateLimit-Limit.
echo
echo "==> the edge middleware (per-tenant limit on new runs)"
if ! curl -fsS --max-time 15 -X PUT "$API/rate-limits/edge" "${auth[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"max_requests":5,"window_seconds":60}' >/tmp/aap-limit.log 2>&1; then
  echo "FAIL: seeding a per-tenant limit failed; the middleware cannot fire without one." >&2
  tail -10 /tmp/aap-limit.log >&2
  exit 1
fi
# The plugin's background loop reloads on an interval; give it one.
sleep 3

saw_plugin_429=0
for _ in $(seq 1 25); do
  headers="$(curl -sS -o /dev/null -D - --max-time 10 \
    -X POST "$API/api/workflows/ai-agent-platform/start" "${auth[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"tenant_id\":\"$TENANT\",\"task\":\"burst\",\"max_steps\":1,\"budget_usd\":1.00}" 2>/dev/null || true)"
  if printf '%s' "$headers" | grep -qi '^HTTP/[0-9.]* 429'; then
    if printf '%s' "$headers" | grep -qi '^x-ratelimit-limit:'; then
      saw_plugin_429=1
      break
    fi
    echo "FAIL: a 429 arrived WITHOUT X-RateLimit-Limit, so it was the CORE limiter," >&2
    echo "not the plugin's edge middleware this hitch point is about." >&2
    printf '%s\n' "$headers" >&2
    exit 1
  fi
  sleep 0.2
done
if (( saw_plugin_429 != 1 )); then
  echo "FAIL: the plugin's rate limit never fired in 25 requests against a" >&2
  echo "      seeded limit of 5 per 60s." >&2
  echo "The likeliest cause is the BACKGROUND LOOP: buckets are built by" >&2
  echo "ratelimiter's reload, which runs plugin.AcrossAllTenants. If that loop is" >&2
  echo "not running or its reload finds no rows, the middleware is registered and" >&2
  echo "inert -- which is the second hitch point failing, not the first." >&2
  exit 1
fi
echo "OK: the plugin's 429 carried X-RateLimit-Limit"

# ---- done --------------------------------------------------------------
echo
echo "==> the documented commands ran ($ran of $total)"
if (( ran != total )); then
  echo "FAIL: only $ran of $total documented commands ran." >&2
  exit 1
fi
echo
echo "PASS: all assertions held."
