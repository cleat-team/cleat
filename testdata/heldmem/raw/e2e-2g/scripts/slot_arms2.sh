#!/bin/bash
export E2E_NFILL=8
cold_arm() { E2E_SKIPFILL=1 E2E_HOLD=20 E2E_LOOPS=3 E2E_SLEEP_MS=9000 E2E_SETTLE_S=20 E2E_TAIL_S=90 arm "$@"; }
arm() { # label db bin flags...
  label=$1; db=$2; bin=$3; shift 3
  echo "##### ARM $label"
  BIN=$bin bash /tmp/e2e_run.sh $db "$@" 2>&1 | grep -v '^{' | grep -E "E2E|oom_kill|memory.peak|OOMKilled|exit state|anon=" | cut -c1-170
  echo "worker fallbacks WARN lines: $(grep -c 'compiling in this process' /tmp/out4377/e2e_$db.worker.log)  peak: $(sed -E 's/.*peak=([0-9]+).*/\1/' /tmp/out4377/e2e_$db.samples | sort -n | tail -1) MiB"
}
cold_arm "2 AFTER (with the headroom refusal), --component-precompile=false, cold start" slot2a /out/slot-after --component-precompile=false
cold_arm "4 AFTER (with the headroom refusal), forced fallback (children time out at 1s), cold start" slot4a /out/slot-after --component-precompile-timeout 1s
cold_arm "5 AFTER (with the headroom refusal), stock flags (precompile ON), cold start" slot5a /out/slot-after
E2E_LOOPS=12 E2E_SLEEP_MS=9000 E2E_HOLD=60 E2E_SETTLE_S=30 E2E_TAIL_S=170 arm "6 AFTER (with the headroom refusal), stock flags, the 9-component worst case" slot6a /out/slot-after
echo ALL-ARMS-DONE
