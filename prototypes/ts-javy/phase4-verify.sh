#!/usr/bin/env bash
# Phase 4 (plan section 5) end to end, from a checkout where bootstrap.sh has already run (worker, cleatctl, javy, the plugin and a
# database exist): build the benchmark workflow in four languages, measure the in-process half and the database half, write
# evidence/phase4/. ~15 minutes. Give it its own database and port (CLEAT_PROTO_*) and run it with nothing else of yours running.
#   CLEAT_PROTO_COMPONENTIZE_PY_DIR=<dir with componentize-py>   Python baseline; without it Python is UNMEASURED and the run fails.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")"
export GOWORK=off GOFLAGS=-mod=mod GOPROXY=off
mkdir -p evidence/phase4
[ -n "${CLEAT_PROTO_COMPONENTIZE_PY_DIR:-}" ] || { echo "UNMEASURED: CLEAT_PROTO_COMPONENTIZE_PY_DIR is not set, so the Python baseline cannot be built"; exit 2; }
step() { local label=$1; shift; python3 phase1_lib.py stop; "$@"; local rc=$?; local left; left=$(python3 phase1_lib.py leak)
  if [ -n "$left" ]; then echo "[$label] LEAK: $left"; rc=3; python3 phase1_lib.py stop; fi; echo "[$label] exit=$rc"; eval "rc_$label=$rc"; }
step build   bash -c "./phase4_build.sh > evidence/phase4/build.txt 2>&1"
step harness bash -c "go build -o .work/bench-harness ./bench/harness 2>&1 | tail -5; test -x .work/bench-harness"
step inproc  bash -c "python3 phase4_measure.py > evidence/phase4/inproc.txt 2>&1"
step db      bash -c "python3 phase4_db.py > evidence/phase4/db.txt 2>&1"
echo "exit codes: build=$rc_build harness=$rc_harness inproc=$rc_inproc db=$rc_db"
[ $rc_build -eq 0 ] && [ $rc_harness -eq 0 ] && [ $rc_inproc -eq 0 ] && [ $rc_db -eq 0 ]
