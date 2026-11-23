#!/bin/bash
# runs INSIDE the 2g container: the worker plus a sampler of the container's own cgroup
DBN=$1; shift
OUT=/out/e2e_$DBN
OWN="postgres://cleat:$(cat /pw_own)@wmem-pg:5432/$DBN?sslmode=disable"
APP="postgres://cleat_app:$(cat /pw)@wmem-pg:5432/$DBN?sslmode=disable"
cg=/sys/fs/cgroup
: > $OUT.samples
( while true; do
    anon=$(awk '/^anon /{print int($2/1048576)}' $cg/memory.stat); file=$(awk '/^file /{print int($2/1048576)}' $cg/memory.stat)
    echo "$(date -u +%H:%M:%S) cur=$(( $(cat $cg/memory.current)/1048576 )) anon=$anon file=$file peak=$(( $(cat $cg/memory.peak)/1048576 )) oom_kill=$(awk '/^oom_kill /{print $2}' $cg/memory.events)" >> $OUT.samples
    sleep 2
  done ) &
exec /out/e2e-worker-def --db "$APP" --migrate-db "$OWN" --migrate-on-start --task-queue e2e-q --poll 100ms --heartbeat 1s "$@" > $OUT.worker.log 2>&1
