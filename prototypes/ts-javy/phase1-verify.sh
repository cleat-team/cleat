#!/usr/bin/env bash
# Phase 1 end to end, from one script so the evidence comes from one run of one tree:
#   Q2 (restart scenarios, history prefix, finally, race)  ->  evidence/phase1/q2-restart.txt
#   Q2 (3 replays of an in-flight history + red controls)  ->  evidence/phase1/q2-replay-3x.txt
#   error and exception paths                              ->  evidence/phase1/errors.txt
#   which journey variants log the short-history warning    ->  evidence/phase1/warn-bisect.txt
# Port, database and container are configurable (CLEAT_PROTO_* , see phase1_lib.py; `python3 phase1_lib.py settings` prints them).
# Needs the Phase 0 toolchain, the cleat-ts-pg container and the artefacts in .work/ (see PHASE1.md). Exits non-zero on any
# failure, and 2 when a check could not be run (UNMEASURED), which is not the same as a finding.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")"
E=evidence/phase1; mkdir -p "$E"
python3 phase1_lib.py stop     # only a worker THIS checkout started (pidfile + this checkout's binary path), never "whatever is on the port"
CLEAT_DB_URL="$(python3 phase1_lib.py owner-dsn)" .work/cleatctl deploy workflow journey .work/journey.wasm 2>&1 | tail -1
rc=0
python3 phase1_verify.py > "$E/q2-restart.txt" 2>&1;  a=$?; tail -2 "$E/q2-restart.txt"
python3 phase1_replay.py > "$E/q2-replay-run.txt" 2>&1; b=$?; tail -1 "$E/q2-replay-run.txt"
python3 phase1_errors.py > "$E/errors.txt" 2>&1;        c=$?; tail -2 "$E/errors.txt"
python3 phase1_warn_bisect.py > "$E/warn-bisect.txt" 2>&1; d=$?; cat "$E/warn-bisect.txt"
echo "exit codes: restart=$a replay=$b errors=$c warn-bisect=$d"
[ $a -eq 0 ] && [ $b -eq 0 ] && [ $c -eq 0 ] && [ $d -eq 0 ]
