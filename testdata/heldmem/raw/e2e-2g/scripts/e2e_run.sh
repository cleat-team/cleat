#!/bin/bash
# usage: e2e_run.sh <dbname> [worker flags...]
DBN=$1; shift
NAME=e2e-$DBN
docker rm -f $NAME >/dev/null 2>&1
docker run -d -e BIN -e SAMPLE_S --name $NAME --network wmem-net --memory ${MEM:-2g} --memory-swap ${MEM:-2g} -v /tmp/out4377:/out -v /tmp/wmem_apppw:/pw:ro -v /tmp/wmem_pgpw:/pw_own:ro -v /tmp/e2e_entry.sh:/entry.sh:ro cleat-py-toolchain bash /entry.sh $DBN "$@" >/dev/null
for i in $(seq 1 60); do grep -q -i -E "polling|started|listening|ready" /tmp/out4377/e2e_$DBN.worker.log 2>/dev/null && break; sleep 1; done
echo "worker up after ~${i}s; first log lines:"; head -c 600 /tmp/out4377/e2e_$DBN.worker.log | cut -c1-200 | head -4
sleep 3
docker run --rm --network wmem-net -v /private/tmp/claude-501/e2e:/e2e -v /tmp/out4377:/out -v /tmp/wmem_pgpw:/pw_own:ro \
  -e E2E_DSN="postgres://cleat:$(cat /tmp/wmem_pgpw)@wmem-pg:5432/$DBN?sslmode=disable" -e E2E_WASM_DIR=/e2e/wasm -e E2E_NFILL -e E2E_HOLD -e E2E_LOOPS -e E2E_SLEEP_MS -e E2E_SETTLE_S -e E2E_TAIL_S -e E2E_SKIPFILL -e E2E_CS -e E2E_VARIANTS \
  cleat-py-toolchain /out/e2e.test -test.run ${E2E_TEST:-TestCombinedWorstCase} -test.v -test.timeout 25m 2>&1 | grep --line-buffered -E "^E2E|--- |FAIL|panic" | tee /tmp/out4377/e2e_$DBN.driver.log
echo "== container end state"
docker exec $NAME bash -c 'cg=/sys/fs/cgroup; echo "memory.peak=$(( $(cat $cg/memory.peak)/1048576 )) MiB memory.current=$(( $(cat $cg/memory.current)/1048576 )) MiB"; grep -E "^(anon|file|shmem|kernel|slab|sock) " $cg/memory.stat | awk "{printf \"%s=%d MiB \", \$1, \$2/1048576}"; echo; cat $cg/memory.events'
docker stop $NAME >/dev/null 2>&1; echo "exit state: $(docker inspect $NAME --format 'OOMKilled={{.State.OOMKilled}} ExitCode={{.State.ExitCode}}')"
docker rm $NAME >/dev/null 2>&1
