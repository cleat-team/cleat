#!/usr/bin/env bash
# Every wasmtime host function must be registered through b.hostFunc.
#
# IMPROVEMENT-PLAN 3.90. b.hostFunc brackets the guest's epoch budget around the
# call (engine/wasmtime_hostbudget.go), so time the host spends on the guest's
# behalf is not charged to the guest's runaway allowance. A host function
# registered with a raw linker.FuncWrap still WORKS -- it just silently charges
# its wait to the guest, which is the defect 3.90 is about. That is the failure
# mode this guard exists for: not a break, a quiet regression.
#
# Re-derive the counts:
#   grep -c "b.hostFunc(linker, " engine/wasmtime_hostfuncs*.go engine/wasmtime_wasi.go
set -euo pipefail

cd "$(dirname "$0")/.."

files=(engine/wasmtime_hostfuncs*.go engine/wasmtime_wasi.go)

raw=$(grep -n "linker\.FuncWrap(" "${files[@]}" || true)
if [ -n "$raw" ]; then
  echo "FAIL: host functions registered without the guest budget bracket."
  echo
  echo "$raw"
  echo
  echo "Register these with b.hostFunc(linker, module, name, fn) instead."
  echo "A raw linker.FuncWrap works, but charges the guest for the time the host"
  echo "spends inside it -- see IMPROVEMENT-PLAN 3.90 and"
  echo "engine/wasmtime_hostbudget.go."
  exit 1
fi

wrapped=$(grep -c "b\.hostFunc(linker, " "${files[@]}" | awk -F: '{s+=$2} END {print s}')

# A guard that matches nothing passes vacuously, which is the same failure this
# repo hit with the ctxWithMem guard (it caught its own drift via checked == 0).
# Anchor on a floor rather than an exact count so adding host functions does not
# require touching this script, but renaming the helper does.
if [ "$wrapped" -lt 50 ]; then
  echo "FAIL: only $wrapped host functions go through b.hostFunc."
  echo "That is far below the ~67 in the tree, so either the helper was renamed"
  echo "and this guard is now checking nothing, or most registrations were"
  echo "changed. Both need a human."
  exit 1
fi

echo "OK: all $wrapped wasmtime host functions are registered through b.hostFunc."
