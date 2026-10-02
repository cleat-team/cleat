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
# role-symmetric with the bare variant as a CONTROL row. Five groups now, on
# both cleat and the DBOS-ISOLATED counterpart specifically:
#
#   tenant code   what a tenant AUTHORS -- the transform/read itself
#   host runner   the plumbing that INVOKES tenant code (dispatch, isolate
#                 setup/teardown) -- not written by the tenant, but written
#                 once per application, not once per platform
#   unit tests    like-for-like test code, counted in the APP TOTAL
#   e2e harness   shown for BOTH sides but EXCLUDED from any total -- see
#                 below for why this is its own group rather than folded
#                 into "unit tests" or dropped
#   platform      what the PLATFORM contributes once, for every tenant step
#                 it will ever run -- reported on ITS OWN LINE, never summed
#                 into the app total, because doing so is exactly the
#                 category-mixing the app/platform split exists to prevent
#
# WHY "unit tests" AND "e2e harness" ARE SEPARATE, not one "tests" group as
# an earlier version of this script had it. cleat's proof of the sandbox
# boundary needs `scripts/run-integration-hub-tenant-sandbox-scenario.sh`
# (~198 lines) -- a real HTTP-deployed worker, receiving a WASM upload
# through the SAME runtime-code-intake path a tenant actually uses. DBOS's
# analogous end-to-end proof lives ENTIRELY inside `isolated-wedge.test.ts`
# (calling `DBOS.startWorkflow` in-process); `run-integration-hub-dbos-
# scenario.sh` (~42 lines) is just an npm install/build/test wrapper with no
# assertions of its own. Counting cleat's harness inside "tests" while DBOS
# has no comparable line to count previously double-counted cleat's cost
# without a DBOS-side counterpart to compare it against. Both harness
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
    print_group "DBOS-isolated: unit tests" \
      "$repo_root/examples/integration-hub-dbos-port/src/isolated-wedge.test.ts"

    echo "== E2E HARNESS, both sides -- shown, NOT summed into either app total (see header comment) =="
    print_group "cleat: e2e harness (drives a real deployed worker over HTTP -- exercises runtime code intake)" \
      "$repo_root/scripts/run-integration-hub-tenant-sandbox-scenario.sh"
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
