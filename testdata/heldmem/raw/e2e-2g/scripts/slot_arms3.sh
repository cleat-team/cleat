#!/bin/bash
export E2E_NFILL=8
cold_arm() { E2E_SKIPFILL=1 E2E_HOLD=20 E2E_LOOPS=3 E2E_SLEEP_MS=9000 E2E_SETTLE_S=20 E2E_TAIL_S=90 arm "$@"; }
arm() { # label db bin flags...
  label=$1; db=$2; bin=$3; shift 3
  echo "##### ARM $label"
  BIN=$bin bash /tmp/e2e_run.sh $db "$@" 2>&1 | grep -v '^{' | grep -E "E2E|oom_kill|memory.peak|OOMKilled|exit state|anon=" | cut -c1-170
  echo "worker fallbacks WARN lines: $(grep -c 'compiling in this process' /tmp/out4377/e2e_$db.worker.log)  peak: $(sed -E 's/.*peak=([0-9]+).*/\1/' /tmp/out4377/e2e_$db.samples | sort -n | tail -1) MiB"
}
cold_arm "2 FINAL, --component-precompile=false (OFF + serialised + headroom), cold start" fa2 /out/slot-after --component-precompile=false
cold_arm "4 FINAL, forced fallback: stock default ON, every child times out at 1s, cold start" fa4 /out/slot-after --component-precompile-timeout 1s
cold_arm "5 FINAL, stock flags, cold start" fa5 /out/slot-after
E2E_LOOPS=12 E2E_SLEEP_MS=9000 E2E_HOLD=60 E2E_SETTLE_S=30 E2E_TAIL_S=170 arm "6 FINAL, stock flags, the 9-component worst case" fa6 /out/slot-after
echo ALL-ARMS-DONE
