#!/usr/bin/env bash
#
# Count code lines for one cleat-vs-DBOS pair, cleat side and DBOS side, with
# the exact same `cloc` invocation on both. cleat#2597.
#
# WHY THIS EXISTS
#
# A number quoted in a README rots the moment either side's source changes,
# and CLAUDE.md's own rule applies directly: "any number you write down
# carries a date and the command that re-derives it." Two sessions counting
# "the same way, slightly differently" -- one adding a --by-file flag, one
# not excluding node_modules, one counting comments and one not -- would
# produce two numbers that look like a discrepancy in the finding rather
# than a discrepancy in the tool invocation. Pinning both invocations in one
# script closes that off.
#
# USAGE
#
#   scripts/dbos-pair-loc.sh order-lifecycle
#   scripts/dbos-pair-loc.sh integration-hub
#   scripts/dbos-pair-loc.sh b2b-saas-control-plane
#
# Adding a pair means adding one case below, not a new script: the whole
# point is one counting rule for every pair this harness ever grows.
#
# order-lifecycle (the CONTROL pair) is app-lines-only on both sides: neither
# side interposes anything between a step and its environment, so there is no
# platform-enforcement asymmetry, and no ROLE asymmetry either -- both sides
# are one workflow file, one server/backend file, one test file.
#
# integration-hub is NOT role-symmetric by file count, and this script has
# been corrected twice by cleat-review's review of #2621 (see git blame):
# once to add the app/platform split, once to make the four roles
# role-symmetric with the bare variant as a CONTROL row, and once more for
# cleat#2642 (the behaviour-assertions row). Six groups now, on both cleat
# and the DBOS-ISOLATED counterpart specifically:
#
#   tenant code   what a tenant AUTHORS -- the transform/read itself
#   host runner   the plumbing that INVOKES tenant code (dispatch, isolate
#                 setup/teardown) -- not written by the tenant, but written
#                 once per application, not once per platform
#   unit tests    the test FILE on each side minus its behaviour assertions
#                 -- counted in the APP TOTAL
#   behaviour     the three tenant behaviours EACH side tests, extracted
#    assertions   from its own file and SUMMED ON BOTH SIDES (cleat#2642).
#                 Before this row, the same work was summed on DBOS's side
#                 and not on cleat's -- cleat's assertions live inside its
#                 e2e harness, a row never summed -- so identical cost
#                 moved the comparison in one direction only.
#   e2e harness   shown for BOTH sides but EXCLUDED from any total -- see
#    machinery    below for why this is its own group rather than folded
#                 into "unit tests" or dropped. cleat's is its harness
#                 script MINUS the behaviour blocks counted above.
#   platform      what the PLATFORM contributes once, for every tenant step
#                 it will ever run -- reported on ITS OWN LINE, never summed
#                 into the app total, because doing so is exactly the
#                 category-mixing the app/platform split exists to prevent
#
# WHY "unit tests" AND "e2e harness" ARE SEPARATE, not one "tests" group as
# an earlier version of this script had it. cleat's proof of the sandbox
# boundary needs `scripts/run-integration-hub-tenant-sandbox-scenario.sh`
# (239 lines in full, of which 85 is the behaviour blocks counted in their
# own row above and 154 the remaining machinery) -- a real HTTP-deployed
# worker, receiving a WASM upload through the SAME runtime-code-intake path
# a tenant actually uses. DBOS's analogous end-to-end proof lives inside
# `isolated-wedge.test.ts` (calling `DBOS.startWorkflow` in-process) -- its
# three behaviour assertions, counted in the row above --
# `run-integration-hub-dbos-scenario.sh` (~42 lines) is just an npm
# install/build/test wrapper with no assertions of its own. Counting
# cleat's harness inside "tests" while DBOS had no comparable line to count
# previously double-counted cleat's cost without a DBOS-side counterpart to
# compare it against. Both harness
# scripts are shown, neither is summed into the app total, and the prose
# says what the size difference is actually about (runtime code intake,
# which only cleat's side exercises) rather than letting a bare number
# imply "DBOS's tests are 3x smaller."
#
# The bare-DBOS.runStep counterpart (src/workflow.ts, src/wedge.test.ts) is
# reported as a separate CONTROL row, excluded from the role-symmetric
# comparison total: it is not what an idiomatic team ships (that is the
# whole reason the isolated-vm counterpart exists), so folding it into the
# "treatment" total would compare a treatment against a treatment-plus-its-
# own-control.
#
# cleat's "tenant code" is two whole files (the tenant-steps mains) because
# that is genuinely all a cleat tenant author writes. Its "host runner" and
# "unit tests" are NOT whole files -- hub.go and hub_test.go do far more
# than this pair's scope (webhook ingestion, connector dispatch; see the
# port's ISSUES.md, "Not a full SyncCustomer port") -- so they are
# STRUCTURALLY EXTRACTED fragments via scripts/dbos-pair-loc-extract.py,
# bounded by the source's own shape (a brace block, a named function) rather
# than a frozen line range that would drift the moment either file is
# edited.
#
# EXIT STATUS
#
#   0  counted; every group's total printed
#   2  UNMEASURED -- cloc or the extractor is not installed/failed, the
#      named pair is unknown, or a pair's files are missing. Never printed
#      as if it were a count of zero.
set -euo pipefail

if ! command -v cloc >/dev/null 2>&1; then
  echo "UNMEASURED: cloc is not installed" >&2
  echo "UNMEASURED: this is a failure of the check, not a finding about either side's size" >&2
  exit 2
fi

pair="${1:-}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
extractor="$repo_root/scripts/dbos-pair-loc-extract.py"

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

extract() {
  # extract <mode> <file> <name-or-regex> <out-basename>
  # Writes the extracted fragment to $scratch/<out-basename>, or exits 2 (via
  # the extractor's own UNMEASURED contract) if the structural marker it
  # looked for is not there.
  #
  # DELIBERATELY NOT `out=$(extract ...)`: bash does not enable
  # `inherit_errexit` by default, so `set -e` is NOT active inside a command
  # substitution's subshell -- a failing python3 there would be silently
  # swallowed and the subshell's last command (an `echo`) would report
  # success regardless. Verified by breaking a marker on purpose: the
  # command-substitution form printed a full report and exited 0 even
  # though the extractor had failed. Calling this as a plain command (not
  # `$(...)`) keeps `set -e` live, so a failure here really does abort the
  # script -- confirmed with the same broken-marker case, which now exits 2.
  local mode="$1" file="$2" name="$3" out="$scratch/$4"
  if ! python3 "$extractor" "$mode" "$file" "$name" > "$out"; then
    exit 2
  fi
}

print_group() {
  local label="$1"
  shift
  if [ "$#" -eq 0 ]; then
    echo "== $label =="
    echo "(no files -- 0 lines, see header comment for why this is not a gap)"
    echo
    return
  fi
  echo "== $label =="
  cloc --quiet "$@"
  echo
}

# remove_blocks <src> <dest> <block>... -- writes <src> with each <block>'s
# text removed, once, verbatim. Used for every "this row is that file MINUS
# the parts counted in another row" computation, so the remainder is derived
# from the blocks themselves rather than from a second, independently-drifting
# line range. Fails UNMEASURED if a block is not found verbatim, because a
# silent no-op removal would put the block in BOTH rows.
remove_blocks() {
  python3 - "$@" <<'EOF'
import sys
src, dest = sys.argv[1], sys.argv[2]
whole = open(src).read()
for extracted_path in sys.argv[3:]:
    block = open(extracted_path).read()
    if block not in whole:
        print(f"UNMEASURED: extracted block from {extracted_path} not found verbatim in {src}", file=sys.stderr)
        sys.exit(2)
    whole = whole.replace(block, '', 1)
open(dest, 'w').write(whole)
EOF
}

case "$pair" in
  order-lifecycle)
    cleat_app_files=(
      "$repo_root/examples/order-lifecycle/order.go"
      "$repo_root/examples/order-lifecycle/backend/main.go"
      "$repo_root/examples/order-lifecycle/order_test.go"
    )
    dbos_app_files=(
      "$repo_root/examples/order-lifecycle-dbos-port/src/workflow.ts"
      "$repo_root/examples/order-lifecycle-dbos-port/src/server.ts"
      "$repo_root/examples/order-lifecycle-dbos-port/src/order.test.ts"
    )
    for f in "${cleat_app_files[@]}" "${dbos_app_files[@]}"; do
      [ -f "$f" ] || { echo "UNMEASURED: expected file is missing: $f" >&2; exit 2; }
    done
    print_group "cleat: app" "${cleat_app_files[@]}"
    print_group "DBOS: app" "${dbos_app_files[@]}"
    ;;

  b2b-saas-control-plane)
    # The same simple shape as order-lifecycle above -- one app section per
    # side, and the pair's whole README-side claim is the single total, so no
    # role breakdown and no extra checker. Registering it in
    # scripts/check-dbos-pair-loc.py is therefore a second dict entry rather
    # than a second parser (contrast integration-hub, whose three role
    # sections needed one).
    cleat_app_files=(
      "$repo_root/examples/b2b-saas-control-plane/provision.go"
      "$repo_root/examples/b2b-saas-control-plane/backend/main.go"
      "$repo_root/examples/b2b-saas-control-plane/provision_test.go"
    )
    dbos_app_files=(
      "$repo_root/examples/b2b-saas-control-plane-dbos-port/src/workflow.ts"
      "$repo_root/examples/b2b-saas-control-plane-dbos-port/src/server.ts"
      "$repo_root/examples/b2b-saas-control-plane-dbos-port/src/provision.test.ts"
    )
    for f in "${cleat_app_files[@]}" "${dbos_app_files[@]}"; do
      [ -f "$f" ] || { echo "UNMEASURED: expected file is missing: $f" >&2; exit 2; }
    done
    print_group "cleat: app" "${cleat_app_files[@]}"
    print_group "DBOS: app" "${dbos_app_files[@]}"
    ;;

  integration-hub)
    hub_go="$repo_root/examples/integration-hub/hub.go"
    hub_test_go="$repo_root/examples/integration-hub/hub_test.go"
    isolated_workflow_ts="$repo_root/examples/integration-hub-dbos-port/src/isolated-workflow.ts"
    # cleat#2642: the two files holding each side's behaviour ASSERTIONS, as
    # opposed to its harness machinery.
    cleat_scenario="$repo_root/scripts/run-integration-hub-tenant-sandbox-scenario.sh"
    isolated_wedge_test="$repo_root/examples/integration-hub-dbos-port/src/isolated-wedge.test.ts"

    for f in "$hub_go" "$hub_test_go" "$isolated_workflow_ts" \
             "$repo_root/examples/integration-hub/tenant-steps/normalize-order/main.go" \
             "$repo_root/examples/integration-hub/tenant-steps/malicious-read-host-file/main.go" \
             "$repo_root/examples/integration-hub/tenant-steps/infinite-loop/main.go" \
             "$repo_root/engine/wasi_policy.go" \
             "$repo_root/engine/wasi_policy_wasmtime.go" \
             "$repo_root/scripts/run-integration-hub-tenant-sandbox-scenario.sh" \
             "$repo_root/scripts/run-integration-hub-dbos-scenario.sh" \
             "$repo_root/examples/integration-hub-dbos-port/src/workflow.ts" \
             "$repo_root/examples/integration-hub-dbos-port/src/wedge.test.ts" \
             "$repo_root/examples/integration-hub-dbos-port/src/isolated-wedge.test.ts"; do
      [ -f "$f" ] || { echo "UNMEASURED: expected file is missing: $f" >&2; exit 2; }
    done

    # Five fragments make up cleat's "host runner", not one: the dispatch
    # block plus the wiring outside it that a search for every "tenant"
    # mention in hub.go turns up (`grep -ni tenant`, minus the file's
    # unrelated per-customer-tenancy header comment and the pre-existing
    # baseline payload line the wedge's own comment says predates it). A
    # brace-block extraction of the dispatch site alone does not see any
    # of these -- coordinator's review of #2621 caught the two struct
    # fields as a real undercount, not a false alarm: TenantStepName,
    # TenantStepRan, its assignment, and the flag's initialisation are all
    # load-bearing (a caller cannot ask for a tenant step, or learn
    # whether one ran, without every one of them) and happened to live
    # outside the block the first extraction looked at.
    extract go-brace-block "$hub_go" 'if in\.TenantStepName' cleat-host-runner-dispatch.go
    extract go-struct-field "$hub_go" TenantStepName cleat-host-runner-field-in.go
    extract go-struct-field "$hub_go" TenantStepRan cleat-host-runner-field-out.go
    extract go-line "$hub_go" '^\s*TenantStepRan:' cleat-host-runner-field-out-set.go
    extract go-line "$hub_go" '^\s*tenantStepRan := false' cleat-host-runner-flag-init.go
    cleat_host_runner="$scratch/cleat-host-runner-dispatch.go"
    cleat_host_runner_field_in="$scratch/cleat-host-runner-field-in.go"
    cleat_host_runner_field_out="$scratch/cleat-host-runner-field-out.go"
    cleat_host_runner_field_out_set="$scratch/cleat-host-runner-field-out-set.go"
    cleat_host_runner_flag_init="$scratch/cleat-host-runner-flag-init.go"
    extract go-func "$hub_test_go" TestSyncCustomer_RunsTheTenantsOwnStep cleat-test1.go
    cleat_test1="$scratch/cleat-test1.go"
    extract go-func "$hub_test_go" TestSyncCustomer_ATenantStepThatFailsNamesItsStep cleat-test2.go
    cleat_test2="$scratch/cleat-test2.go"
    extract ts-const-template "$isolated_workflow_ts" NORMALIZE_ORDER_SOURCE dbos-tenant1.ts
    dbos_tenant1="$scratch/dbos-tenant1.ts"
    extract ts-const-template "$isolated_workflow_ts" READ_HOST_FILE_SOURCE dbos-tenant2.ts
    dbos_tenant2="$scratch/dbos-tenant2.ts"
    # cleat#2628: a THIRD tenant behaviour, added after the other two. Its
    # own template literal, extracted the same way -- if this were left in
    # the "host runner" remainder below instead, tenant code would be
    # miscounted as infrastructure, which is exactly the asymmetry this
    # script exists to catch in the OTHER direction.
    extract ts-const-template "$isolated_workflow_ts" INFINITE_LOOP_SOURCE dbos-tenant3.ts
    dbos_tenant3="$scratch/dbos-tenant3.ts"

    # cleat#2642: the BEHAVIOUR ASSERTIONS -- the three tenant behaviours each
    # side tests -- extracted into ONE group, summed into BOTH app totals.
    #
    # Before this, a behaviour's test cost landed in cleat's e2e harness (a row
    # deliberately NOT summed) and in DBOS's unit-test file (a row that IS), so
    # work that cost the two sides the SAME moved the comparison by its full
    # size in one direction only. The two extractions are mechanically
    # different because the harnesses are in different languages; they are the
    # same three behaviours, which is what makes this row like-for-like where
    # the whole files are not.
    extract sh-banner-block "$cleat_scenario" '^# ---- the positive control' cleat-behaviour-1.sh
    extract sh-banner-block "$cleat_scenario" '^# ---- the adversarial case' cleat-behaviour-2.sh
    extract sh-banner-block "$cleat_scenario" '^# ---- the bilateral bound' cleat-behaviour-3.sh
    cleat_behaviour1="$scratch/cleat-behaviour-1.sh"
    cleat_behaviour2="$scratch/cleat-behaviour-2.sh"
    cleat_behaviour3="$scratch/cleat-behaviour-3.sh"
    extract ts-func "$isolated_wedge_test" testPositiveControlNormalizeOrderSucceedsThroughIsolate dbos-behaviour-1.ts
    extract ts-func "$isolated_wedge_test" testReadHostFileIsRefusedByTheIsolate dbos-behaviour-2.ts
    extract ts-func "$isolated_wedge_test" testRunawayLoopIsInterruptedByTheTimeout dbos-behaviour-3.ts
    dbos_behaviour1="$scratch/dbos-behaviour-1.ts"
    dbos_behaviour2="$scratch/dbos-behaviour-2.ts"
    dbos_behaviour3="$scratch/dbos-behaviour-3.ts"

    # Each side's MACHINERY is its assertions file with the behaviour blocks
    # removed, so nothing is counted in two rows.
    remove_blocks "$cleat_scenario" "$scratch/cleat-harness-machinery.sh" \
      "$cleat_behaviour1" "$cleat_behaviour2" "$cleat_behaviour3"
    remove_blocks "$isolated_wedge_test" "$scratch/dbos-unit-machinery.ts" \
      "$dbos_behaviour1" "$dbos_behaviour2" "$dbos_behaviour3"

    # isolated-workflow.ts's "host runner" is everything in the file EXCEPT
    # the three tenant-code template literals just extracted above -- computed
    # by removing those blocks' TEXT from a copy of the file, rather
    # than by a second, independently-drifting line range.
    python3 - "$isolated_workflow_ts" "$dbos_tenant1" "$dbos_tenant2" "$dbos_tenant3" > "$scratch/dbos-host-runner.ts" <<'EOF'
import sys
whole = open(sys.argv[1]).read()
for extracted_path in sys.argv[2:]:
    block = open(extracted_path).read()
    if block not in whole:
        print(f"UNMEASURED: extracted block from {extracted_path} not found verbatim in {sys.argv[1]}", file=sys.stderr)
        sys.exit(2)
    whole = whole.replace(block, '', 1)
sys.stdout.write(whole)
EOF

    echo "== CONTROL (excluded from the comparison total): bare DBOS.runStep, no sandbox =="
    cloc --quiet \
      "$repo_root/examples/integration-hub-dbos-port/src/workflow.ts" \
      "$repo_root/examples/integration-hub-dbos-port/src/wedge.test.ts"
    echo

    print_group "cleat: tenant code" \
      "$repo_root/examples/integration-hub/tenant-steps/normalize-order/main.go" \
      "$repo_root/examples/integration-hub/tenant-steps/malicious-read-host-file/main.go" \
      "$repo_root/examples/integration-hub/tenant-steps/infinite-loop/main.go"
    print_group "cleat: host runner (extracted from hub.go -- dispatch block + every other TenantStepName-related line)" \
      "$cleat_host_runner" "$cleat_host_runner_field_in" "$cleat_host_runner_field_out" \
      "$cleat_host_runner_field_out_set" "$cleat_host_runner_flag_init"
    print_group "cleat: unit tests (extracted from hub_test.go)" "$cleat_test1" "$cleat_test2"
    print_group "DBOS-isolated: tenant code (extracted template literals)" "$dbos_tenant1" "$dbos_tenant2" "$dbos_tenant3"
    print_group "DBOS-isolated: host runner (isolated-workflow.ts minus tenant code)" "$scratch/dbos-host-runner.ts"
    print_group "DBOS-isolated: unit tests (isolated-wedge.test.ts -- its three behaviour functions are counted in the behaviour row below)" \
      "$scratch/dbos-unit-machinery.ts"

    echo "== BEHAVIOUR ASSERTIONS, both sides -- SUMMED INTO BOTH APP TOTALS (cleat#2642) =="
    print_group "cleat: behaviour assertions (the three behaviour blocks of the e2e harness)" \
      "$cleat_behaviour1" "$cleat_behaviour2" "$cleat_behaviour3"
    print_group "DBOS-isolated: behaviour assertions (the three test functions of isolated-wedge.test.ts)" \
      "$dbos_behaviour1" "$dbos_behaviour2" "$dbos_behaviour3"

    echo "== E2E HARNESS MACHINERY, both sides -- shown, NOT summed into either app total (see header comment) =="
    print_group "cleat: e2e harness machinery (the harness minus its three behaviour blocks -- drives a real deployed worker over HTTP)" \
      "$scratch/cleat-harness-machinery.sh"
    print_group "DBOS-isolated: e2e harness (npm install/build/test wrapper -- the assertions live in the unit test above; no runtime code intake to exercise)" \
      "$repo_root/scripts/run-integration-hub-dbos-scenario.sh"

    print_group "cleat: platform (own line -- never summed into the app total)" \
      "$repo_root/engine/wasi_policy.go" \
      "$repo_root/engine/wasi_policy_wasmtime.go"
    print_group "DBOS-isolated: platform (own line -- never summed into the app total)" # empty -- see header comment
    ;;

  *)
    echo "usage: $0 order-lifecycle|integration-hub" >&2
    exit 2
    ;;
esac
